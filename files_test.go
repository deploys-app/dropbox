package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/acoshift/pgsql/pgctx"
	"gocloud.dev/blob"
)

func TestFileHandler_Success(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	bkt := newTestBucket(t)
	app := newTestApp(bkt, authorized)

	bw, err := bkt.NewWriter(t.Context(), "testfile", &blob.WriterOptions{
		CacheControl:       "public, max-age=86400",
		ContentDisposition: `attachment; filename="hello.txt"`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bw.Write([]byte("hello world")); err != nil {
		t.Fatal(err)
	}
	if err := bw.Close(); err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest(http.MethodGet, "/files/testfile", nil)
	r = r.WithContext(db.Ctx())
	r.SetPathValue("fn", "testfile")
	w := httptest.NewRecorder()
	app.fileHandler(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := w.Body.String(); got != "hello world" {
		t.Errorf("body = %q, want %q", got, "hello world")
	}
	if cc := w.Header().Get("Cache-Control"); cc != "public, max-age=86400" {
		t.Errorf("Cache-Control = %q", cc)
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.Contains(cd, "hello.txt") {
		t.Errorf("Content-Disposition = %q", cd)
	}
	if cl := w.Header().Get("Content-Length"); cl != "11" {
		t.Errorf("Content-Length = %q, want 11", cl)
	}
}

func TestFileHandler_NotFound(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	app := newTestApp(newTestBucket(t), authorized)

	r := httptest.NewRequest(http.MethodGet, "/files/doesnotexist", nil)
	r = r.WithContext(db.Ctx())
	r.SetPathValue("fn", "doesnotexist")
	w := httptest.NewRecorder()
	app.fileHandler(w, r)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestFileHandler_RouteIntegration(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	bkt := newTestBucket(t)
	app := newTestApp(bkt, authorized)

	bw, err := bkt.NewWriter(t.Context(), "routefile", nil)
	if err != nil {
		t.Fatal(err)
	}
	bw.Write([]byte("route test"))
	if err := bw.Close(); err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest(http.MethodGet, "/files/routefile", nil)
	r = r.WithContext(db.Ctx())
	w := httptest.NewRecorder()
	app.routes().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
}

func TestFileHandler_NoHeadersWhenAttrsEmpty(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	bkt := newTestBucket(t)
	app := newTestApp(bkt, authorized)

	bw, err := bkt.NewWriter(t.Context(), "plain", nil)
	if err != nil {
		t.Fatal(err)
	}
	bw.Write([]byte("plain"))
	if err := bw.Close(); err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest(http.MethodGet, "/files/plain", nil)
	r = r.WithContext(db.Ctx())
	r.SetPathValue("fn", "plain")
	w := httptest.NewRecorder()
	app.fileHandler(w, r)

	if cd := w.Header().Get("Content-Disposition"); cd != "" {
		t.Errorf("Content-Disposition = %q, want empty when none set", cd)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "" {
		t.Errorf("Cache-Control = %q, want empty when none set", cc)
	}
	if cl := w.Header().Get("Content-Length"); cl != "5" {
		t.Errorf("Content-Length = %q, want 5", cl)
	}
}

func TestLookupFileProject_FromDB(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	ctx := db.Ctx()

	if _, err := pgctx.Exec(ctx, `
		INSERT INTO files (fn, project_id, size, filename, ttl, expires_at)
		VALUES ('lookupfile', 'proj-xyz', 1, 'x', 1, now() + interval '1 day')
	`); err != nil {
		t.Fatal(err)
	}

	if got := lookupFileProject(ctx, "lookupfile"); got != "proj-xyz" {
		t.Errorf("lookupFileProject = %q, want proj-xyz", got)
	}
	// Second call exercises the cache hit path.
	if got := lookupFileProject(ctx, "lookupfile"); got != "proj-xyz" {
		t.Errorf("lookupFileProject (cached) = %q, want proj-xyz", got)
	}
}

func TestLookupFileProject_NotFound(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)

	if got := lookupFileProject(db.Ctx(), "missingfile"); got != "" {
		t.Errorf("lookupFileProject = %q, want empty for missing fn", got)
	}
}
