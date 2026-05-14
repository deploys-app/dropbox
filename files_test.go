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
	bkt := newTestBucket(t)
	app := newTestApp(bkt, authorized)
	fn := "fhs-" + t.Name()

	bw, err := bkt.NewWriter(t.Context(), fn, &blob.WriterOptions{
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

	r := httptest.NewRequest(http.MethodGet, "/files/"+fn, nil)
	r = r.WithContext(testDB.Ctx())
	r.SetPathValue("fn", fn)
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
	app := newTestApp(newTestBucket(t), authorized)
	fn := "fhn-" + t.Name()

	r := httptest.NewRequest(http.MethodGet, "/files/"+fn, nil)
	r = r.WithContext(testDB.Ctx())
	r.SetPathValue("fn", fn)
	w := httptest.NewRecorder()
	app.fileHandler(w, r)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestFileHandler_RouteIntegration(t *testing.T) {
	t.Parallel()
	bkt := newTestBucket(t)
	app := newTestApp(bkt, authorized)
	fn := "fhr-" + t.Name()

	bw, err := bkt.NewWriter(t.Context(), fn, nil)
	if err != nil {
		t.Fatal(err)
	}
	bw.Write([]byte("route test"))
	if err := bw.Close(); err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest(http.MethodGet, "/files/"+fn, nil)
	r = r.WithContext(testDB.Ctx())
	w := httptest.NewRecorder()
	app.routes().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
}

func TestFileHandler_NoHeadersWhenAttrsEmpty(t *testing.T) {
	t.Parallel()
	bkt := newTestBucket(t)
	app := newTestApp(bkt, authorized)
	fn := "fhne-" + t.Name()

	bw, err := bkt.NewWriter(t.Context(), fn, nil)
	if err != nil {
		t.Fatal(err)
	}
	bw.Write([]byte("plain"))
	if err := bw.Close(); err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest(http.MethodGet, "/files/"+fn, nil)
	r = r.WithContext(testDB.Ctx())
	r.SetPathValue("fn", fn)
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
	ctx := testDB.Ctx()
	fn := "lfp-" + t.Name()
	projectID := "proj-" + t.Name()

	if _, err := pgctx.Exec(ctx, `
		INSERT INTO files (fn, project_id, size, filename, ttl, expires_at)
		VALUES ($1, $2, 1, 'x', 1, now() + interval '1 day')
	`, fn, projectID); err != nil {
		t.Fatal(err)
	}

	if got := lookupFileProject(ctx, fn); got != projectID {
		t.Errorf("lookupFileProject = %q, want %q", got, projectID)
	}
	// Second call exercises the cache hit path.
	if got := lookupFileProject(ctx, fn); got != projectID {
		t.Errorf("lookupFileProject (cached) = %q, want %q", got, projectID)
	}
}

func TestLookupFileProject_NotFound(t *testing.T) {
	t.Parallel()
	ctx := testDB.Ctx()
	fn := "lfp-missing-" + t.Name()

	if got := lookupFileProject(ctx, fn); got != "" {
		t.Errorf("lookupFileProject = %q, want empty for missing fn", got)
	}
}
