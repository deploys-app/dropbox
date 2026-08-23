package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/acoshift/pgsql/pgctx"
	"gocloud.dev/blob"
)

func TestFileHandler_Success(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	bkt := newTestBucket(t)
	app := newTestApp(bkt, authorized)

	fn := validTestFn("testfile")
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

	r := httptest.NewRequest(http.MethodGet, "/files/"+signedToken(fn), nil)
	r = r.WithContext(db.Ctx())
	r.SetPathValue("token", signedToken(fn))
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

	fn := validTestFn("notexist")
	r := httptest.NewRequest(http.MethodGet, "/files/"+signedToken(fn), nil)
	r = r.WithContext(db.Ctx())
	r.SetPathValue("token", signedToken(fn))
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

	fn := validTestFn("routefile")
	bw, err := bkt.NewWriter(t.Context(), fn, nil)
	if err != nil {
		t.Fatal(err)
	}
	bw.Write([]byte("route test"))
	if err := bw.Close(); err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest(http.MethodGet, "/files/"+signedToken(fn), nil)
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

	fn := validTestFn("plain")
	bw, err := bkt.NewWriter(t.Context(), fn, nil)
	if err != nil {
		t.Fatal(err)
	}
	bw.Write([]byte("plain"))
	if err := bw.Close(); err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest(http.MethodGet, "/files/"+signedToken(fn), nil)
	r = r.WithContext(db.Ctx())
	r.SetPathValue("token", signedToken(fn))
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

// TestFileHandler_Range exercises streamFile's http.ServeContent range
// handling across the common forms: prefix, mid, open-ended, suffix, full
// (no Range), and an unsatisfiable range. The shared db/bucket/app are safe
// because the subtests run sequentially and each writes a distinct fn.
func TestFileHandler_Range(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	bkt := newTestBucket(t)
	app := newTestApp(bkt, authorized)

	const body = "hello world" // 11 bytes

	cases := []struct {
		name       string
		rangeHdr   string
		wantStatus int
		wantBody   string
		wantCR     string // expected Content-Range ("" = don't check)
		wantCL     string // expected Content-Length ("" = don't check)
	}{
		{"prefix", "bytes=0-4", http.StatusPartialContent, "hello", "bytes 0-4/11", "5"},
		{"middle", "bytes=6-8", http.StatusPartialContent, "wor", "bytes 6-8/11", "3"},
		{"openEnded", "bytes=6-", http.StatusPartialContent, "world", "bytes 6-10/11", "5"},
		{"suffix", "bytes=-5", http.StatusPartialContent, "world", "bytes 6-10/11", "5"},
		{"full", "", http.StatusOK, body, "", "11"},
		{"unsatisfiable", "bytes=100-200", http.StatusRequestedRangeNotSatisfiable, "", "bytes */11", ""},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fn := validTestFn(fmt.Sprintf("rng%d", i))
			writeObject(t, bkt, fn, "text/plain", []byte(body))

			r := httptest.NewRequest(http.MethodGet, "/files/"+signedToken(fn), nil)
			r = r.WithContext(db.Ctx())
			r.SetPathValue("token", signedToken(fn))
			if tc.rangeHdr != "" {
				r.Header.Set("Range", tc.rangeHdr)
			}
			w := httptest.NewRecorder()
			app.fileHandler(w, r)

			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", w.Code, tc.wantStatus)
			}
			if tc.wantBody != "" && w.Body.String() != tc.wantBody {
				t.Errorf("body = %q, want %q", w.Body.String(), tc.wantBody)
			}
			if tc.wantCR != "" {
				if cr := w.Header().Get("Content-Range"); cr != tc.wantCR {
					t.Errorf("Content-Range = %q, want %q", cr, tc.wantCR)
				}
			}
			if tc.wantCL != "" {
				if cl := w.Header().Get("Content-Length"); cl != tc.wantCL {
					t.Errorf("Content-Length = %q, want %q", cl, tc.wantCL)
				}
			}
			// Both 200 and 206 advertise range support; ServeContent omits
			// Accept-Ranges only on the 416.
			if tc.wantStatus != http.StatusRequestedRangeNotSatisfiable {
				if ar := w.Header().Get("Accept-Ranges"); ar != "bytes" {
					t.Errorf("Accept-Ranges = %q, want bytes", ar)
				}
			}
		})
	}
}

// TestCDNFileHandler_Range confirms range support is also live on the
// unauthenticated _cdn origin path the CDN edge fetches from.
func TestCDNFileHandler_Range(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	bkt := newTestBucket(t)
	app := newTestApp(bkt, authorized)

	fn := validTestFn("cdnrng")
	writeObject(t, bkt, fn, "text/plain", []byte("hello world"))

	r := httptest.NewRequest(http.MethodGet, "/_cdn/"+signedToken(fn), nil)
	r = r.WithContext(db.Ctx())
	r.SetPathValue("token", signedToken(fn))
	r.Header.Set("Range", "bytes=6-")
	w := httptest.NewRecorder()
	app.cdnFileHandler(w, r)

	if w.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", w.Code)
	}
	if got := w.Body.String(); got != "world" {
		t.Errorf("body = %q, want %q", got, "world")
	}
	if cr := w.Header().Get("Content-Range"); cr != "bytes 6-10/11" {
		t.Errorf("Content-Range = %q, want %q", cr, "bytes 6-10/11")
	}
}

func TestLookupFile_FromDB(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	ctx := db.Ctx()

	if _, err := pgctx.Exec(ctx, `
		INSERT INTO files (fn, project_id, size, filename, ttl, expires_at)
		VALUES ('lookupfile', 'proj-xyz', 1, 'x', 1, now() + interval '1 day')
	`); err != nil {
		t.Fatal(err)
	}

	got := lookupFile(ctx, "lookupfile")
	if !got.Found {
		t.Fatal("Found = false, want true")
	}
	if got.ProjectID != "proj-xyz" {
		t.Errorf("ProjectID = %q, want proj-xyz", got.ProjectID)
	}
	if got.Expired() {
		t.Errorf("Expired() = true, want false (expires_at is in the future)")
	}
	// Second call exercises the cache hit path.
	if got := lookupFile(ctx, "lookupfile"); got.ProjectID != "proj-xyz" {
		t.Errorf("ProjectID (cached) = %q, want proj-xyz", got.ProjectID)
	}
}

func TestLookupFile_NotFound(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)

	got := lookupFile(db.Ctx(), "missingfile")
	if got.Found {
		t.Errorf("Found = true, want false for missing fn")
	}
	if got.ProjectID != "" {
		t.Errorf("ProjectID = %q, want empty for missing fn", got.ProjectID)
	}
	if got.Expired() {
		t.Errorf("Expired() = true, want false when not found (nothing to enforce)")
	}
}

func TestLookupFile_SingleflightCollapsesConcurrentCalls(t *testing.T) {
	// 50 goroutines race on the same cold-cache fn. sf.Do must collapse
	// them into a single Postgres SELECT — anything more and the DDoS
	// shield against a thundering herd at the cache-miss edge is gone.
	t.Parallel()
	db := newTestDB(t)
	ctx := db.Ctx()

	fn := "sf-thundering-herd-fn"
	if _, err := pgctx.Exec(ctx, `
		INSERT INTO files (fn, project_id, size, filename, ttl, expires_at)
		VALUES ($1, 'proj-sf', 1, 'x', 1, now() + interval '1 day')
	`, fn); err != nil {
		t.Fatal(err)
	}

	const N = 50
	results := make([]fileMeta, N)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range N {
		wg.Go(func() {
			<-start // release all goroutines at once to maximise overlap
			results[i] = lookupFile(ctx, fn)
		})
	}
	close(start)
	wg.Wait()

	// Correctness: every caller got the same correct result.
	for i, r := range results {
		if !r.Found || r.ProjectID != "proj-sf" {
			t.Errorf("results[%d] = %+v, want Found=true ProjectID=proj-sf", i, r)
		}
	}

	// Dedupe: sf collapsed the herd. We allow >1 because the per-caller
	// cache check before sf.Do can race in a way that lets a few
	// stragglers issue their own queries when the first one finishes
	// fast and the cache populates before later goroutines reach sf —
	// but it must be far below N.
	if got := lookupFileDBQueryCount(fn); got >= N {
		t.Errorf("DB queries for %s = %d, want <%d (singleflight should have collapsed the herd)", fn, got, N)
	}
}

func TestLookupFile_Expired(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	ctx := db.Ctx()

	if _, err := pgctx.Exec(ctx, `
		INSERT INTO files (fn, project_id, size, filename, ttl, expires_at)
		VALUES ('expiredmeta', 'proj-xyz', 1, 'x', 1, now() - interval '1 hour')
	`); err != nil {
		t.Fatal(err)
	}

	got := lookupFile(ctx, "expiredmeta")
	if !got.Found {
		t.Fatal("Found = false, want true")
	}
	if !got.Expired() {
		t.Errorf("Expired() = false, want true (expires_at is in the past)")
	}
}

func TestFileHandler_ExpiredReturnsGone(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	bkt := newTestBucket(t)
	app := newTestApp(bkt, authorized)

	// Object still in the bucket — GC hasn't run yet — but the DB row
	// says it expired an hour ago.
	fn := validTestFn("expiredfile")
	bw, err := bkt.NewWriter(t.Context(), fn, nil)
	if err != nil {
		t.Fatal(err)
	}
	bw.Write([]byte("stale bytes"))
	if err := bw.Close(); err != nil {
		t.Fatal(err)
	}

	ctx := db.Ctx()
	if _, err := pgctx.Exec(ctx, `
		INSERT INTO files (fn, project_id, size, filename, ttl, expires_at)
		VALUES ($1, 'proj-xyz', 11, 'x', 1, now() - interval '1 hour')
	`, fn); err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest(http.MethodGet, "/files/"+signedToken(fn), nil)
	r = r.WithContext(ctx)
	r.SetPathValue("token", signedToken(fn))
	w := httptest.NewRecorder()
	app.fileHandler(w, r)

	if w.Code != http.StatusGone {
		t.Fatalf("status = %d, want 410", w.Code)
	}
	if body := w.Body.String(); strings.Contains(body, "stale bytes") {
		t.Errorf("body leaked object content: %q", body)
	}
}

func TestFileHandler_ExpiredSkipsBucket(t *testing.T) {
	// Expired DB row, *empty bucket* — proves the expired check runs
	// before Bucket.Attributes. Under DDoS this is the difference between
	// every request burning a GCS Class B operation and every request
	// being a cheap cache hit. If the order regresses, this test starts
	// returning 404 (bucket-NotFound) instead of 410.
	t.Parallel()
	db := newTestDB(t)
	app := newTestApp(newTestBucket(t), authorized)

	fn := validTestFn("expiredghost")
	ctx := db.Ctx()
	if _, err := pgctx.Exec(ctx, `
		INSERT INTO files (fn, project_id, size, filename, ttl, expires_at)
		VALUES ($1, 'proj-xyz', 99, 'x', 1, now() - interval '1 hour')
	`, fn); err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest(http.MethodGet, "/files/"+signedToken(fn), nil)
	r = r.WithContext(ctx)
	r.SetPathValue("token", signedToken(fn))
	w := httptest.NewRecorder()
	app.fileHandler(w, r)

	if w.Code != http.StatusGone {
		t.Fatalf("status = %d, want 410 (expired check must run before bucket call)", w.Code)
	}
}

func TestCDNFileHandler_ExpiredSkipsBucket(t *testing.T) {
	// Same DDoS-protection assertion for the /_cdn/{fn} origin path.
	t.Parallel()
	db := newTestDB(t)
	app := newTestApp(newTestBucket(t), authorized)
	app.CDNBaseURL = "https://cdn.example.com/"

	fn := validTestFn("expiredghostcdn")
	ctx := db.Ctx()
	if _, err := pgctx.Exec(ctx, `
		INSERT INTO files (fn, project_id, size, filename, ttl, expires_at)
		VALUES ($1, 'proj-xyz', 99, 'x', 1, now() - interval '1 hour')
	`, fn); err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest(http.MethodGet, "/_cdn/"+signedToken(fn), nil)
	r = r.WithContext(ctx)
	w := httptest.NewRecorder()
	app.routes().ServeHTTP(w, r)

	if w.Code != http.StatusGone {
		t.Fatalf("status = %d, want 410 (expired check must run before bucket call)", w.Code)
	}
}

func TestFileHandler_ExpiredOverridesCDNRedirect(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	bkt := newTestBucket(t)
	app := newTestApp(bkt, authorized)
	app.CDNBaseURL = "https://cdn.example.com/"

	fn := validTestFn("expiredcdn")
	bw, err := bkt.NewWriter(t.Context(), fn, nil)
	if err != nil {
		t.Fatal(err)
	}
	bw.Write([]byte("stale"))
	if err := bw.Close(); err != nil {
		t.Fatal(err)
	}

	ctx := db.Ctx()
	if _, err := pgctx.Exec(ctx, `
		INSERT INTO files (fn, project_id, size, filename, ttl, expires_at)
		VALUES ($1, 'proj-xyz', 5, 'x', 1, now() - interval '1 hour')
	`, fn); err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest(http.MethodGet, "/files/"+signedToken(fn), nil)
	r = r.WithContext(ctx)
	r.SetPathValue("token", signedToken(fn))
	w := httptest.NewRecorder()
	app.fileHandler(w, r)

	if w.Code != http.StatusGone {
		t.Fatalf("status = %d, want 410 (must not redirect to CDN)", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "" {
		t.Errorf("Location = %q, want empty (no redirect for expired files)", loc)
	}
}

func TestCDNFileHandler_ExpiredReturnsGone(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	bkt := newTestBucket(t)
	app := newTestApp(bkt, authorized)
	app.CDNBaseURL = "https://cdn.example.com/"

	fn := validTestFn("expiredorigin")
	bw, err := bkt.NewWriter(t.Context(), fn, nil)
	if err != nil {
		t.Fatal(err)
	}
	bw.Write([]byte("stale origin"))
	if err := bw.Close(); err != nil {
		t.Fatal(err)
	}

	ctx := db.Ctx()
	if _, err := pgctx.Exec(ctx, `
		INSERT INTO files (fn, project_id, size, filename, ttl, expires_at)
		VALUES ($1, 'proj-xyz', 12, 'x', 1, now() - interval '1 hour')
	`, fn); err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest(http.MethodGet, "/_cdn/"+signedToken(fn), nil)
	r = r.WithContext(ctx)
	w := httptest.NewRecorder()
	app.routes().ServeHTTP(w, r)

	if w.Code != http.StatusGone {
		t.Fatalf("status = %d, want 410 (CDN origin must refuse expired files)", w.Code)
	}
	if body := w.Body.String(); strings.Contains(body, "stale origin") {
		t.Errorf("body leaked object content to CDN edge: %q", body)
	}
}

func TestFileHandler_CDNRedirect(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	bkt := newTestBucket(t)
	app := newTestApp(bkt, authorized)
	app.CDNBaseURL = "https://cdn.example.com/"

	fn := validTestFn("cdnfile")
	bw, err := bkt.NewWriter(t.Context(), fn, nil)
	if err != nil {
		t.Fatal(err)
	}
	bw.Write([]byte("hello world"))
	if err := bw.Close(); err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest(http.MethodGet, "/files/"+signedToken(fn), nil)
	r = r.WithContext(db.Ctx())
	r.SetPathValue("token", signedToken(fn))
	w := httptest.NewRecorder()
	app.fileHandler(w, r)

	if w.Code != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want 307", w.Code)
	}
	// Redirect preserves the *full* signed token so the CDN's
	// origin-fetch hits cdnFileHandler with a URL we can re-verify.
	if want := "https://cdn.example.com/" + signedToken(fn); w.Header().Get("Location") != want {
		t.Errorf("Location = %q, want %q", w.Header().Get("Location"), want)
	}
	// The default http.Redirect body is `<a href=...>Temporary Redirect</a>` —
	// it must not contain the object bytes.
	if got := w.Body.String(); strings.Contains(got, "hello world") {
		t.Errorf("body streamed the object instead of redirecting: %q", got)
	}
}

func TestFileHandler_CDNInternalClientStreams(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	bkt := newTestBucket(t)
	app := newTestApp(bkt, authorized)
	app.CDNBaseURL = "https://cdn.example.com/"

	fn := validTestFn("internalfile")
	bw, err := bkt.NewWriter(t.Context(), fn, nil)
	if err != nil {
		t.Fatal(err)
	}
	bw.Write([]byte("internal bytes"))
	if err := bw.Close(); err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest(http.MethodGet, "/files/"+signedToken(fn), nil)
	r = r.WithContext(db.Ctx())
	r.SetPathValue("token", signedToken(fn))
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
	app.CDNBaseURL = "https://cdn.example.com/"

	fn := validTestFn("publicfile")
	bw, err := bkt.NewWriter(t.Context(), fn, nil)
	if err != nil {
		t.Fatal(err)
	}
	bw.Write([]byte("public"))
	if err := bw.Close(); err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest(http.MethodGet, "/files/"+signedToken(fn), nil)
	r = r.WithContext(db.Ctx())
	r.SetPathValue("token", signedToken(fn))
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
	app.CDNBaseURL = "https://cdn.example.com/"

	fn := validTestFn("origin")
	bw, err := bkt.NewWriter(t.Context(), fn, &blob.WriterOptions{
		CacheControl: "public, max-age=86400",
	})
	if err != nil {
		t.Fatal(err)
	}
	bw.Write([]byte("origin bytes"))
	if err := bw.Close(); err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest(http.MethodGet, "/_cdn/"+signedToken(fn), nil)
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
	app.CDNBaseURL = "https://cdn.example.com/"

	fn := validTestFn("cdnnope")
	r := httptest.NewRequest(http.MethodGet, "/_cdn/"+signedToken(fn), nil)
	r = r.WithContext(db.Ctx())
	w := httptest.NewRecorder()
	app.routes().ServeHTTP(w, r)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
	// Bucket NotFound for a signed token → cache the negative at the
	// edge so the same dead URL doesn't keep hitting origin.
	if cc := w.Header().Get("Cache-Control"); cc != "public, max-age=3600" {
		t.Errorf("Cache-Control = %q, want public, max-age=3600", cc)
	}
}

func TestCDNFileHandler_SuccessCacheControlUsesRemainingTTL(t *testing.T) {
	// The CDN response must cap max-age at the file's remaining TTL so
	// the edge stops serving past expires_at. fn is unique per upload
	// so we mark the body immutable.
	t.Parallel()
	db := newTestDB(t)
	bkt := newTestBucket(t)
	app := newTestApp(bkt, authorized)
	app.CDNBaseURL = "https://cdn.example.com/"

	fn := validTestFn("cdncc")
	bw, err := bkt.NewWriter(t.Context(), fn, &blob.WriterOptions{
		CacheControl: "public, max-age=86400", // bucket default — must be overridden
	})
	if err != nil {
		t.Fatal(err)
	}
	bw.Write([]byte("data"))
	if err := bw.Close(); err != nil {
		t.Fatal(err)
	}

	ctx := db.Ctx()
	if _, err := pgctx.Exec(ctx, `
		INSERT INTO files (fn, project_id, size, filename, ttl, expires_at)
		VALUES ($1, 'proj-cc', 4, 'x', 7, now() + interval '7 days')
	`, fn); err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest(http.MethodGet, "/_cdn/"+signedToken(fn), nil)
	r = r.WithContext(ctx)
	w := httptest.NewRecorder()
	app.routes().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	cc := w.Header().Get("Cache-Control")
	if !strings.HasPrefix(cc, "public, max-age=") || !strings.HasSuffix(cc, ", immutable") {
		t.Fatalf("Cache-Control = %q, want shape 'public, max-age=N, immutable'", cc)
	}
	// Extract and sanity-check the max-age — must reflect ~7 days, not
	// the bucket default of 86400.
	var maxAge int
	if _, err := fmt.Sscanf(cc, "public, max-age=%d, immutable", &maxAge); err != nil {
		t.Fatalf("parse max-age from %q: %v", cc, err)
	}
	const sevenDays = 7 * 24 * 3600
	if maxAge < sevenDays-3600 || maxAge > sevenDays {
		t.Errorf("max-age = %d, want roughly 7 days (%d ± 1h)", maxAge, sevenDays)
	}
}

func TestCDNFileHandler_ExpiredCacheControl(t *testing.T) {
	// 410 response must carry a short Cache-Control so the edge stops
	// asking origin for an expired URL that's still in circulation.
	t.Parallel()
	db := newTestDB(t)
	bkt := newTestBucket(t)
	app := newTestApp(bkt, authorized)
	app.CDNBaseURL = "https://cdn.example.com/"

	fn := validTestFn("cdnccexp")
	bw, _ := bkt.NewWriter(t.Context(), fn, nil)
	bw.Write([]byte("stale"))
	bw.Close()

	ctx := db.Ctx()
	if _, err := pgctx.Exec(ctx, `
		INSERT INTO files (fn, project_id, size, filename, ttl, expires_at)
		VALUES ($1, 'proj-cc', 5, 'x', 1, now() - interval '1 hour')
	`, fn); err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest(http.MethodGet, "/_cdn/"+signedToken(fn), nil)
	r = r.WithContext(ctx)
	w := httptest.NewRecorder()
	app.routes().ServeHTTP(w, r)

	if w.Code != http.StatusGone {
		t.Fatalf("status = %d, want 410", w.Code)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "public, max-age=3600" {
		t.Errorf("Cache-Control = %q, want public, max-age=3600", cc)
	}
}

func TestCDNFileHandler_InvalidTokenNoCacheControl(t *testing.T) {
	// Forgeable garbage tokens must NOT get a Cache-Control — each
	// token is a unique URL so caching wouldn't reduce origin load and
	// would just fill edge slots on attacker traffic.
	t.Parallel()
	app := newTestApp(newTestBucket(t), authorized)
	app.CDNBaseURL = "https://cdn.example.com/"

	r := httptest.NewRequest(http.MethodGet, "/_cdn/garbage", nil)
	w := httptest.NewRecorder()
	app.routes().ServeHTTP(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "" {
		t.Errorf("Cache-Control = %q, want empty (attacker traffic shouldn't be cached)", cc)
	}
}

func TestFileHandler_InvalidTokenRejected(t *testing.T) {
	// Tokens that don't HMAC-verify must 404 from parseToken without
	// touching DB or GCS. The test app has no pgctx attached, so if the
	// signature shortcut ever regresses, lookupFile will panic and the
	// test blows up loudly — which is the right failure mode for the
	// primary DDoS shield going away.
	t.Parallel()
	app := newTestApp(newTestBucket(t), authorized)

	goodFn := validTestFn("good")
	goodToken := signedToken(goodFn)
	tamperedSig := goodToken[:len(goodToken)-1] + "0"
	if tamperedSig == goodToken {
		tamperedSig = goodToken[:len(goodToken)-1] + "1"
	}
	forgedDiffKey := makeToken([]byte("not-the-real-key"), goodFn)

	cases := map[string]string{
		"empty":            "",
		"short":            "short",
		"path-traversal":   "../../etc/passwd",
		"no-separator":     strings.Repeat("a", fnLen+sigLen),
		"empty-fn":         tokenSep + signFilename(testSignKey, ""),
		"empty-sig":        goodFn + tokenSep,
		"tampered-sig":     tamperedSig,
		"forged-other-key": forgedDiffKey,
	}
	for name, token := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest(http.MethodGet, "/files/x", nil)
			r.SetPathValue("token", token)
			w := httptest.NewRecorder()
			app.fileHandler(w, r)
			if w.Code != http.StatusNotFound {
				t.Errorf("token=%q: status = %d, want 404", token, w.Code)
			}
		})
	}
}

func TestCDNFileHandler_InvalidTokenRejected(t *testing.T) {
	t.Parallel()
	app := newTestApp(newTestBucket(t), authorized)
	app.CDNBaseURL = "https://cdn.example.com/"

	r := httptest.NewRequest(http.MethodGet, "/_cdn/short", nil)
	w := httptest.NewRecorder()
	app.routes().ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestFileHandler_BucketMissNegativeCached(t *testing.T) {
	// First request misses everywhere → 404 and caches BucketMissing.
	// Then we write the object out-of-band into the bucket. The second
	// request must still 404, proving Bucket.Attributes was not called
	// — that's the DDoS protection for a flood against a known-bad fn
	// (e.g. a URL the attacker held after expiry+GC).
	t.Parallel()
	db := newTestDB(t)
	bkt := newTestBucket(t)
	app := newTestApp(bkt, authorized)

	fn := validTestFn("missneg")

	r1 := httptest.NewRequest(http.MethodGet, "/files/"+signedToken(fn), nil)
	r1 = r1.WithContext(db.Ctx())
	r1.SetPathValue("token", signedToken(fn))
	w1 := httptest.NewRecorder()
	app.fileHandler(w1, r1)
	if w1.Code != http.StatusNotFound {
		t.Fatalf("first request: status = %d, want 404", w1.Code)
	}

	bw, err := bkt.NewWriter(t.Context(), fn, nil)
	if err != nil {
		t.Fatal(err)
	}
	bw.Write([]byte("late arrival"))
	if err := bw.Close(); err != nil {
		t.Fatal(err)
	}

	r2 := httptest.NewRequest(http.MethodGet, "/files/"+signedToken(fn), nil)
	r2 = r2.WithContext(db.Ctx())
	r2.SetPathValue("token", signedToken(fn))
	w2 := httptest.NewRecorder()
	app.fileHandler(w2, r2)
	if w2.Code != http.StatusNotFound {
		t.Fatalf("second request: status = %d, want 404 (negative cache must mask the out-of-band write)", w2.Code)
	}
	if strings.Contains(w2.Body.String(), "late arrival") {
		t.Errorf("body leaked out-of-band object: %q", w2.Body.String())
	}
}

func TestCDNFileHandler_BucketMissNegativeCached(t *testing.T) {
	// Same assertion for /_cdn/{fn}. The CDN origin needs the same
	// shield since the edge can hammer it on cache misses.
	t.Parallel()
	db := newTestDB(t)
	bkt := newTestBucket(t)
	app := newTestApp(bkt, authorized)
	app.CDNBaseURL = "https://cdn.example.com/"

	fn := validTestFn("missnegcdn")

	r1 := httptest.NewRequest(http.MethodGet, "/_cdn/"+signedToken(fn), nil)
	r1 = r1.WithContext(db.Ctx())
	w1 := httptest.NewRecorder()
	app.routes().ServeHTTP(w1, r1)
	if w1.Code != http.StatusNotFound {
		t.Fatalf("first request: status = %d, want 404", w1.Code)
	}

	bw, err := bkt.NewWriter(t.Context(), fn, nil)
	if err != nil {
		t.Fatal(err)
	}
	bw.Write([]byte("late arrival"))
	if err := bw.Close(); err != nil {
		t.Fatal(err)
	}

	r2 := httptest.NewRequest(http.MethodGet, "/_cdn/"+signedToken(fn), nil)
	r2 = r2.WithContext(db.Ctx())
	w2 := httptest.NewRecorder()
	app.routes().ServeHTTP(w2, r2)
	if w2.Code != http.StatusNotFound {
		t.Fatalf("second request: status = %d, want 404 (negative cache must mask the out-of-band write)", w2.Code)
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
