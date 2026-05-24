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

func TestFileHandler_CDNRedirect(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	bkt := newTestBucket(t)
	app := newTestApp(bkt, authorized)
	app.CDNDomain = "cdn.example.com"

	bw, err := bkt.NewWriter(t.Context(), "cdnfile", nil)
	if err != nil {
		t.Fatal(err)
	}
	bw.Write([]byte("hello world"))
	if err := bw.Close(); err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest(http.MethodGet, "/files/cdnfile", nil)
	r = r.WithContext(db.Ctx())
	r.SetPathValue("fn", "cdnfile")
	w := httptest.NewRecorder()
	app.fileHandler(w, r)

	if w.Code != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want 307", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "https://cdn.example.com/cdnfile" {
		t.Errorf("Location = %q, want https://cdn.example.com/cdnfile", loc)
	}
	if got := w.Body.Len(); got > 100 {
		// the default redirect body is small ("<a href=...>"); we shouldn't
		// be streaming the object.
		t.Errorf("body length = %d, expected a short redirect body", got)
	}
}

func TestFileHandler_CDNInternalClientStreams(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	bkt := newTestBucket(t)
	app := newTestApp(bkt, authorized)
	app.CDNDomain = "cdn.example.com"

	bw, err := bkt.NewWriter(t.Context(), "internalfile", nil)
	if err != nil {
		t.Fatal(err)
	}
	bw.Write([]byte("internal bytes"))
	if err := bw.Close(); err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest(http.MethodGet, "/files/internalfile", nil)
	r = r.WithContext(db.Ctx())
	r.SetPathValue("fn", "internalfile")
	r.Header.Set("X-Real-Ip", "10.0.0.5") // private IP -> internal
	w := httptest.NewRecorder()
	app.fileHandler(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (internal callers stream)", w.Code)
	}
	if got := w.Body.String(); got != "internal bytes" {
		t.Errorf("body = %q, want %q", got, "internal bytes")
	}
}

func TestFileHandler_CDNRedirectPublicXRealIP(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	bkt := newTestBucket(t)
	app := newTestApp(bkt, authorized)
	app.CDNDomain = "cdn.example.com"

	bw, err := bkt.NewWriter(t.Context(), "publicfile", nil)
	if err != nil {
		t.Fatal(err)
	}
	bw.Write([]byte("public"))
	if err := bw.Close(); err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest(http.MethodGet, "/files/publicfile", nil)
	r = r.WithContext(db.Ctx())
	r.SetPathValue("fn", "publicfile")
	r.Header.Set("X-Real-Ip", "203.0.113.5") // public IP -> CDN path
	w := httptest.NewRecorder()
	app.fileHandler(w, r)

	if w.Code != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want 307 (public callers redirect)", w.Code)
	}
}

func TestCDNFileHandler_Streams(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	bkt := newTestBucket(t)
	app := newTestApp(bkt, authorized)
	app.CDNDomain = "cdn.example.com"

	bw, err := bkt.NewWriter(t.Context(), "origin", &blob.WriterOptions{
		CacheControl: "public, max-age=86400",
	})
	if err != nil {
		t.Fatal(err)
	}
	bw.Write([]byte("origin bytes"))
	if err := bw.Close(); err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest(http.MethodGet, "/_cdn/files/origin", nil)
	r = r.WithContext(db.Ctx())
	w := httptest.NewRecorder()
	app.routes().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := w.Body.String(); got != "origin bytes" {
		t.Errorf("body = %q, want %q", got, "origin bytes")
	}
	if cc := w.Header().Get("Cache-Control"); cc != "public, max-age=86400" {
		t.Errorf("Cache-Control = %q", cc)
	}
}

func TestCDNFileHandler_NotFound(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	app := newTestApp(newTestBucket(t), authorized)
	app.CDNDomain = "cdn.example.com"

	r := httptest.NewRequest(http.MethodGet, "/_cdn/files/nope", nil)
	r = r.WithContext(db.Ctx())
	w := httptest.NewRecorder()
	app.routes().ServeHTTP(w, r)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestIsInternalClient(t *testing.T) {
	t.Parallel()
	cases := []struct {
		ip   string
		want bool
	}{
		{"", false},
		{"not-an-ip", false},
		{"10.0.0.1", true},
		{"172.16.5.5", true},
		{"192.168.1.1", true},
		{"127.0.0.1", true},
		{"169.254.1.1", true},
		{"203.0.113.5", false},
		{"8.8.8.8", false},
	}
	for _, tc := range cases {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if tc.ip != "" {
			r.Header.Set("X-Real-Ip", tc.ip)
		}
		if got := isInternalClient(r); got != tc.want {
			t.Errorf("isInternalClient(%q) = %v, want %v", tc.ip, got, tc.want)
		}
	}
}
