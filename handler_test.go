package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gocloud.dev/blob"
	"gocloud.dev/blob/memblob"
)

func newTestBucket(t *testing.T) *blob.Bucket {
	t.Helper()
	bkt := memblob.OpenBucket(nil)
	t.Cleanup(func() { bkt.Close() })
	return bkt
}

// writeObject stores body under fn in bkt with the given content type
// (pass "" for none). Mirrors what uploadHandler writes, minus metadata.
func writeObject(t *testing.T, bkt *blob.Bucket, fn, contentType string, body []byte) {
	t.Helper()
	bw, err := bkt.NewWriter(t.Context(), fn, &blob.WriterOptions{ContentType: contentType})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := bw.Close(); err != nil {
		t.Fatal(err)
	}
}

// validTestFn returns an fnLen-char fn drawn from fnAlphabet, with the
// descriptive suffix preserved so failing tests are still readable.
// Distinct suffixes produce distinct fns, which keeps parallel tests
// isolated in the in-process per-fn cache.
func validTestFn(suffix string) string {
	if len(suffix) > fnLen {
		panic("validTestFn: suffix too long: " + suffix)
	}
	return strings.Repeat("a", fnLen-len(suffix)) + suffix
}

// testSignKey is the fixed HMAC key used in every test app. Tests build
// signed tokens with makeToken(testSignKey, fn) and the handlers verify
// against this same key.
var testSignKey = []byte("test-sign-key-do-not-use-in-prod")

// signedToken wraps a test fn into the public URL token by HMAC-signing
// it with testSignKey. Tests put this in the URL path; the handler
// parses it back into fn for the bucket/DB lookup.
func signedToken(fn string) string {
	return makeToken(testSignKey, fn)
}

// tokenToFn is the inverse: pull the token off a downloadUrl, verify
// it, and return the fn that's actually stored in the bucket and DB.
// Tests use this when they need to inspect the upload's bucket object.
func tokenToFn(t *testing.T, downloadURL string) string {
	t.Helper()
	token := strings.TrimPrefix(downloadURL, "https://example.com/")
	fn, ok := parseToken(testSignKey, token)
	if !ok {
		t.Fatalf("downloadUrl %q does not contain a valid signed token", downloadURL)
	}
	return fn
}

func countObjects(t *testing.T, bkt *blob.Bucket) int {
	t.Helper()
	iter := bkt.List(nil)
	count := 0
	for {
		if _, err := iter.Next(context.Background()); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		count++
	}
	return count
}

func authorized(_ context.Context, _, _, _ string) AuthResult {
	return AuthResult{Authorized: true, Project: Project{ID: "proj-1", Project: "test"}}
}

func unauthorized(_ context.Context, _, _, _ string) AuthResult {
	return AuthResult{}
}

func newTestApp(bkt *blob.Bucket, authFn func(context.Context, string, string, string) AuthResult) *App {
	return &App{
		Bucket:    bkt,
		BaseURL:   "https://example.com/",
		SignKey:   testSignKey,
		checkAuth: authFn,
	}
}

// uploadResp is the shape of a successful upload response.
type uploadResp struct {
	OK     bool `json:"ok"`
	Result struct {
		DownloadURL string `json:"downloadUrl"`
		ExpiresAt   string `json:"expiresAt"`
	} `json:"result"`
}

// failResp is the shape of an error response.
type failResp struct {
	OK    bool `json:"ok"`
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
}

func TestGetRoot(t *testing.T) {
	t.Parallel()
	app := newTestApp(newTestBucket(t), authorized)
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	app.routes().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Deploys.app Dropbox Service") {
		t.Errorf("unexpected body: %q", w.Body.String())
	}
}

func TestNonRootPath(t *testing.T) {
	t.Parallel()
	app := newTestApp(newTestBucket(t), authorized)
	r := httptest.NewRequest(http.MethodGet, "/other", nil)
	w := httptest.NewRecorder()
	app.routes().ServeHTTP(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestRoot_MethodNotAllowed(t *testing.T) {
	t.Parallel()
	app := newTestApp(newTestBucket(t), authorized)
	r := httptest.NewRequest(http.MethodPut, "/", nil)
	w := httptest.NewRecorder()
	app.routes().ServeHTTP(w, r)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", w.Code)
	}
}

func TestUpload_EmptyBody(t *testing.T) {
	t.Parallel()
	app := newTestApp(newTestBucket(t), authorized)
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.ContentLength = 0
	w := httptest.NewRecorder()
	app.uploadHandler(w, r)

	var resp failResp
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.OK {
		t.Error("expected ok=false for empty body")
	}
	if resp.Error.Message != "body empty" {
		t.Errorf("message = %q", resp.Error.Message)
	}
}

func TestUpload_NilBody(t *testing.T) {
	t.Parallel()
	app := newTestApp(newTestBucket(t), authorized)
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.Body = nil
	w := httptest.NewRecorder()
	app.uploadHandler(w, r)

	var resp failResp
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.OK {
		t.Error("expected ok=false for nil body")
	}
	if resp.Error.Message != "body empty" {
		t.Errorf("message = %q", resp.Error.Message)
	}
}

func TestUpload_EmptyChunkedBody(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	bkt := newTestBucket(t)
	app := newTestApp(bkt, authorized)

	// ContentLength == -1 mimics a chunked / unknown-length request: the early
	// r.ContentLength == 0 guard does not fire, so emptiness is only detected
	// after io.Copy streams zero bytes. The handler must still reject it and
	// leave nothing behind — no bucket object, no DB row.
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(""))
	r = r.WithContext(db.Ctx())
	r.ContentLength = -1
	w := httptest.NewRecorder()
	app.uploadHandler(w, r)

	var resp failResp
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.OK {
		t.Error("expected ok=false for empty chunked body")
	}
	if resp.Error.Message != "body empty" {
		t.Errorf("message = %q, want body empty", resp.Error.Message)
	}
	if n := countObjects(t, bkt); n != 0 {
		t.Errorf("bucket objects = %d, want 0 (empty upload must not leave an object)", n)
	}
	if n := db.CountFiles(t); n != 0 {
		t.Errorf("db files = %d, want 0 (empty upload must not write a row)", n)
	}
}

func TestUpload_ChunkedNonEmptyBodySucceeds(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	bkt := newTestBucket(t)
	app := newTestApp(bkt, authorized)

	// The same unknown-length path must still accept a non-empty body: the
	// n == 0 guard keys off bytes actually streamed, not ContentLength.
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("hello"))
	r = r.WithContext(db.Ctx())
	r.ContentLength = -1
	w := httptest.NewRecorder()
	app.uploadHandler(w, r)

	var resp uploadResp
	json.NewDecoder(w.Body).Decode(&resp)
	if !resp.OK {
		t.Fatal("expected ok=true for non-empty chunked body")
	}
	if n := countObjects(t, bkt); n != 1 {
		t.Errorf("bucket objects = %d, want 1", n)
	}
	if n := db.CountFiles(t); n != 1 {
		t.Errorf("db files = %d, want 1", n)
	}
}

func TestUpload_Success(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	bkt := newTestBucket(t)
	app := newTestApp(bkt, authorized)

	body := strings.NewReader("hello world")
	r := httptest.NewRequest(http.MethodPost, "/", body)
	r = r.WithContext(db.Ctx())
	r.ContentLength = int64(len("hello world"))
	w := httptest.NewRecorder()
	app.uploadHandler(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var resp uploadResp
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK {
		t.Fatal("expected ok=true")
	}
	if !strings.HasPrefix(resp.Result.DownloadURL, "https://example.com/") {
		t.Errorf("downloadUrl = %q, want prefix https://example.com/", resp.Result.DownloadURL)
	}

	expiresAt, err := time.Parse(time.RFC3339, resp.Result.ExpiresAt)
	if err != nil {
		t.Fatalf("parse expiresAt: %v", err)
	}
	diff := time.Until(expiresAt)
	if diff < 23*time.Hour || diff > 25*time.Hour {
		t.Errorf("expiresAt diff = %v, want ~24h", diff)
	}
	if n := countObjects(t, bkt); n != 1 {
		t.Errorf("bucket objects = %d, want 1", n)
	}
	if n := db.CountFiles(t); n != 1 {
		t.Errorf("db files = %d, want 1", n)
	}

	// The signed token in the URL must be persisted verbatim so the api's
	// dropbox.List (which has no SignKey) can rebuild the same download URL.
	wantToken := strings.TrimPrefix(resp.Result.DownloadURL, "https://example.com/")
	var gotToken string
	if err := db.DB.QueryRowContext(context.Background(), `SELECT token FROM files`).Scan(&gotToken); err != nil {
		t.Fatal(err)
	}
	if gotToken != wantToken {
		t.Errorf("stored token = %q, want %q (matching downloadUrl)", gotToken, wantToken)
	}
}

func TestUpload_TTL(t *testing.T) {
	t.Parallel()
	cases := []struct {
		ttl     string
		wantTTL int
	}{
		{"0", 1},
		{"1", 1},
		{"3", 3},
		{"7", 7},
		{"8", 1},
		{"-1", 1},
		{"abc", 1},
		{"", 1},
	}
	for _, tc := range cases {
		t.Run("ttl="+tc.ttl, func(t *testing.T) {
			t.Parallel()
			db := newTestDB(t)
			app := newTestApp(newTestBucket(t), authorized)

			url := "/"
			if tc.ttl != "" {
				url = "/?ttl=" + tc.ttl
			}
			body := strings.NewReader("data")
			r := httptest.NewRequest(http.MethodPost, url, body)
			r = r.WithContext(db.Ctx())
			r.ContentLength = 4
			w := httptest.NewRecorder()
			app.uploadHandler(w, r)

			var resp uploadResp
			json.NewDecoder(w.Body).Decode(&resp)
			if !resp.OK {
				t.Fatal("expected ok=true")
			}
			expiresAt, err := time.Parse(time.RFC3339, resp.Result.ExpiresAt)
			if err != nil {
				t.Fatalf("parse expiresAt: %v", err)
			}
			wantDiff := time.Duration(tc.wantTTL) * 24 * time.Hour
			gotDiff := time.Until(expiresAt)
			if math.Abs(float64(gotDiff-wantDiff)) > float64(time.Hour) {
				t.Errorf("expiresAt diff = %v, want ~%v", gotDiff, wantDiff)
			}
		})
	}
}

func TestUpload_TTLFromHeader(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	app := newTestApp(newTestBucket(t), authorized)

	body := strings.NewReader("data")
	r := httptest.NewRequest(http.MethodPost, "/", body)
	r = r.WithContext(db.Ctx())
	r.ContentLength = 4
	r.Header.Set("param-ttl", "5")
	w := httptest.NewRecorder()
	app.uploadHandler(w, r)

	var resp uploadResp
	json.NewDecoder(w.Body).Decode(&resp)
	expiresAt, _ := time.Parse(time.RFC3339, resp.Result.ExpiresAt)
	if diff := time.Until(expiresAt); math.Abs(float64(diff-5*24*time.Hour)) > float64(time.Hour) {
		t.Errorf("expiresAt diff = %v, want ~120h (TTL 5 from header)", diff)
	}
}

func TestUpload_QueryParamOverridesHeader(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	app := newTestApp(newTestBucket(t), authorized)

	body := strings.NewReader("data")
	r := httptest.NewRequest(http.MethodPost, "/?ttl=6", body)
	r = r.WithContext(db.Ctx())
	r.ContentLength = 4
	r.Header.Set("param-ttl", "2")
	w := httptest.NewRecorder()
	app.uploadHandler(w, r)

	var resp uploadResp
	json.NewDecoder(w.Body).Decode(&resp)
	expiresAt, _ := time.Parse(time.RFC3339, resp.Result.ExpiresAt)
	if diff := time.Until(expiresAt); math.Abs(float64(diff-6*24*time.Hour)) > float64(time.Hour) {
		t.Errorf("expiresAt diff = %v, want ~144h (TTL 6 from query param)", diff)
	}
}

func TestUpload_FilenameFromQuery(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	bkt := newTestBucket(t)
	app := newTestApp(bkt, authorized)

	body := strings.NewReader("data")
	r := httptest.NewRequest(http.MethodPost, "/?filename=report.pdf", body)
	r = r.WithContext(db.Ctx())
	r.ContentLength = 4
	w := httptest.NewRecorder()
	app.uploadHandler(w, r)

	var resp uploadResp
	json.NewDecoder(w.Body).Decode(&resp)
	if !resp.OK {
		t.Fatal("expected ok=true")
	}

	fn := tokenToFn(t, resp.Result.DownloadURL)
	attrs, err := bkt.Attributes(t.Context(), fn)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(attrs.ContentDisposition, `filename="report.pdf"`) {
		t.Errorf("bucket Content-Disposition = %q, want to contain filename=\"report.pdf\"", attrs.ContentDisposition)
	}
}

func TestUpload_FilenameFromHeader(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	bkt := newTestBucket(t)
	app := newTestApp(bkt, authorized)

	body := strings.NewReader("data")
	r := httptest.NewRequest(http.MethodPost, "/", body)
	r = r.WithContext(db.Ctx())
	r.ContentLength = 4
	r.Header.Set("param-filename", "report.pdf")
	w := httptest.NewRecorder()
	app.uploadHandler(w, r)

	var resp uploadResp
	json.NewDecoder(w.Body).Decode(&resp)
	if !resp.OK {
		t.Fatal("expected ok=true")
	}

	fn := tokenToFn(t, resp.Result.DownloadURL)
	attrs, err := bkt.Attributes(t.Context(), fn)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(attrs.ContentDisposition, `filename="report.pdf"`) {
		t.Errorf("bucket Content-Disposition = %q, want to contain filename=\"report.pdf\"", attrs.ContentDisposition)
	}
}

func TestUpload_FilenameWithQuotesEscaped(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	bkt := newTestBucket(t)
	app := newTestApp(bkt, authorized)

	body := strings.NewReader("data")
	r := httptest.NewRequest(http.MethodPost, `/?filename=say+"hi".txt`, body)
	r = r.WithContext(db.Ctx())
	r.ContentLength = 4
	w := httptest.NewRecorder()
	app.uploadHandler(w, r)

	var resp uploadResp
	json.NewDecoder(w.Body).Decode(&resp)
	if !resp.OK {
		t.Fatal("expected ok=true")
	}

	fn := tokenToFn(t, resp.Result.DownloadURL)
	attrs, err := bkt.Attributes(t.Context(), fn)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(attrs.ContentDisposition, `"`) != 2 {
		t.Errorf("Content-Disposition should have exactly 2 quote chars (the wrapping ones), got %q", attrs.ContentDisposition)
	}
}

func TestUpload_NoFilenameNoContentDisposition(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	bkt := newTestBucket(t)
	app := newTestApp(bkt, authorized)

	body := strings.NewReader("data")
	r := httptest.NewRequest(http.MethodPost, "/", body)
	r = r.WithContext(db.Ctx())
	r.ContentLength = 4
	w := httptest.NewRecorder()
	app.uploadHandler(w, r)

	var resp uploadResp
	json.NewDecoder(w.Body).Decode(&resp)
	fn := tokenToFn(t, resp.Result.DownloadURL)
	attrs, err := bkt.Attributes(t.Context(), fn)
	if err != nil {
		t.Fatal(err)
	}
	if attrs.ContentDisposition != "" {
		t.Errorf("Content-Disposition = %q, want empty when no filename provided", attrs.ContentDisposition)
	}
	if attrs.CacheControl != "public, max-age=86400" {
		t.Errorf("Cache-Control = %q, want public, max-age=86400", attrs.CacheControl)
	}
}

func TestUpload_Unauthorized(t *testing.T) {
	t.Parallel()
	app := newTestApp(newTestBucket(t), unauthorized)

	body := strings.NewReader("data")
	r := httptest.NewRequest(http.MethodPost, "/?project=myproject", body)
	r.ContentLength = 4
	r.Header.Set("Authorization", "Bearer bad-token")
	w := httptest.NewRecorder()
	app.uploadHandler(w, r)

	var resp failResp
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.OK {
		t.Error("expected ok=false")
	}
	if resp.Error.Message != "api: unauthorized" {
		t.Errorf("message = %q", resp.Error.Message)
	}
}

func TestUpload_ProjectIDParamRouting(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)

	var gotProject, gotProjectID string
	authFn := func(_ context.Context, _, project, projectID string) AuthResult {
		gotProject, gotProjectID = project, projectID
		return AuthResult{Authorized: true, Project: Project{ID: "proj-1"}}
	}
	app := newTestApp(newTestBucket(t), authFn)

	body := strings.NewReader("data")
	r := httptest.NewRequest(http.MethodPost, "/?projectId=pid-from-query", body)
	r = r.WithContext(db.Ctx())
	r.ContentLength = 4
	r.Header.Set("param-project", "p-from-header")
	w := httptest.NewRecorder()
	app.uploadHandler(w, r)

	if gotProjectID != "pid-from-query" {
		t.Errorf("projectId passed to auth = %q, want pid-from-query", gotProjectID)
	}
	if gotProject != "p-from-header" {
		t.Errorf("project passed to auth = %q, want p-from-header (header fallback)", gotProject)
	}
}

func TestUpload_ProjectParamRouting(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)

	var gotProject, gotProjectID string
	authFn := func(_ context.Context, _, project, projectID string) AuthResult {
		gotProject, gotProjectID = project, projectID
		return AuthResult{Authorized: true, Project: Project{ID: "proj-1"}}
	}
	app := newTestApp(newTestBucket(t), authFn)

	// ?project= carries the project sid; it must reach auth as `project`
	// (me.authorized resolves it by sid) and must not be confused with the
	// numeric `projectId`.
	body := strings.NewReader("data")
	r := httptest.NewRequest(http.MethodPost, "/?project=acme-sid", body)
	r = r.WithContext(db.Ctx())
	r.ContentLength = 4
	r.Header.Set("param-project-id", "id-from-header")
	w := httptest.NewRecorder()
	app.uploadHandler(w, r)

	if gotProject != "acme-sid" {
		t.Errorf("project passed to auth = %q, want acme-sid", gotProject)
	}
	if gotProjectID != "id-from-header" {
		t.Errorf("projectId passed to auth = %q, want id-from-header (header fallback)", gotProjectID)
	}
}

func TestUpload_ProjectNumericRoutesToID(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)

	var gotProject, gotProjectID string
	authFn := func(_ context.Context, _, project, projectID string) AuthResult {
		gotProject, gotProjectID = project, projectID
		return AuthResult{Authorized: true, Project: Project{ID: "proj-1"}}
	}
	app := newTestApp(newTestBucket(t), authFn)

	// An all-digit ?project= is a numeric project ID, not a sid (sids start
	// with a letter), so it must reach auth as projectID, leaving project empty.
	body := strings.NewReader("data")
	r := httptest.NewRequest(http.MethodPost, "/?project=12345", body)
	r = r.WithContext(db.Ctx())
	r.ContentLength = 4
	w := httptest.NewRecorder()
	app.uploadHandler(w, r)

	if gotProjectID != "12345" {
		t.Errorf("projectId passed to auth = %q, want 12345", gotProjectID)
	}
	if gotProject != "" {
		t.Errorf("project passed to auth = %q, want empty (numeric routed to projectId)", gotProject)
	}
}

func TestUpload_ExplicitProjectIDWinsOverNumericProject(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)

	var gotProject, gotProjectID string
	authFn := func(_ context.Context, _, project, projectID string) AuthResult {
		gotProject, gotProjectID = project, projectID
		return AuthResult{Authorized: true, Project: Project{ID: "proj-1"}}
	}
	app := newTestApp(newTestBucket(t), authFn)

	// An explicit ?projectId= must not be clobbered by the numeric-project
	// shortcut; project is left as-is (me.authorized prioritizes projectId).
	body := strings.NewReader("data")
	r := httptest.NewRequest(http.MethodPost, "/?project=999&projectId=42", body)
	r = r.WithContext(db.Ctx())
	r.ContentLength = 4
	w := httptest.NewRecorder()
	app.uploadHandler(w, r)

	if gotProjectID != "42" {
		t.Errorf("projectId passed to auth = %q, want 42 (explicit wins)", gotProjectID)
	}
	if gotProject != "999" {
		t.Errorf("project passed to auth = %q, want 999 (left untouched)", gotProject)
	}
}

func TestIsAllDigits(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		"":           false,
		"12345":      true,
		"0":          true,
		"acme":       false,
		"acme-prod":  false,
		"12a":        false,
		"a123":       false,
		" 12":        false,
		"12 ":        false,
		"-1":         false,
		"1.0":        false,
	}
	for in, want := range cases {
		if got := isAllDigits(in); got != want {
			t.Errorf("isAllDigits(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestGenerateFilename(t *testing.T) {
	t.Parallel()
	a, b := generateFilename(), generateFilename()
	if a == b {
		t.Error("expected unique filenames")
	}
	if len(a) != fnLen {
		t.Errorf("filename length = %d, want %d", len(a), fnLen)
	}
	// Must be strictly alphanumeric — no special chars at all.
	for _, c := range a {
		switch {
		case c >= '0' && c <= '9':
		case c >= 'A' && c <= 'Z':
		case c >= 'a' && c <= 'z':
		default:
			t.Errorf("filename contains non-alphanumeric char %q: %s", c, a)
		}
	}
}

func TestGenerateFilename_DistributionLooksUniform(t *testing.T) {
	t.Parallel()
	// Rough check that rejection sampling doesn't bias the alphabet.
	// With 1000 * fnLen = 24000 chars over 62 symbols, expected count
	// per symbol is ~387. We just assert every symbol appears at least
	// once, which would fail catastrophically if a whole half of the
	// alphabet got dropped (e.g. by an off-by-one in the reject bound).
	counts := map[byte]int{}
	for range 1000 {
		for j := 0; j < len(generateFilename()); j++ {
			counts[generateFilename()[j]]++
		}
	}
	for _, c := range []byte(fnAlphabet) {
		if counts[c] == 0 {
			t.Errorf("symbol %q never appeared in 1000 generated fns", c)
		}
	}
}

func TestSignFilename_Deterministic(t *testing.T) {
	t.Parallel()
	fn := validTestFn("sigtest")
	a := signFilename(testSignKey, fn)
	b := signFilename(testSignKey, fn)
	if a != b {
		t.Errorf("signFilename not deterministic: %q vs %q", a, b)
	}
	if len(a) != sigLen {
		t.Errorf("sig length = %d, want %d", len(a), sigLen)
	}
}

func TestSignFilename_KeyMatters(t *testing.T) {
	t.Parallel()
	fn := validTestFn("keytest")
	a := signFilename(testSignKey, fn)
	b := signFilename([]byte("different-key"), fn)
	if a == b {
		t.Errorf("sig should differ across keys; got %q for both", a)
	}
}

func TestParseToken_RoundTripsGeneratedToken(t *testing.T) {
	t.Parallel()
	for range 16 {
		fn := generateFilename()
		token := makeToken(testSignKey, fn)
		if want := fnLen + 1 + sigLen; len(token) != want {
			t.Fatalf("token length = %d, want %d", len(token), want)
		}
		if !strings.Contains(token, tokenSep) {
			t.Fatalf("token %q missing separator %q", token, tokenSep)
		}
		got, ok := parseToken(testSignKey, token)
		if !ok || got != fn {
			t.Errorf("parseToken(%q) = (%q, %v), want (%q, true)", token, got, ok, fn)
		}
	}
}

func TestParseToken_AcceptsDifferentFnLengths(t *testing.T) {
	// The whole point of the separator: parseToken must work for fns of
	// any length, as long as the sig was produced by signFilename. This
	// guards future changes to fnLen without breaking outstanding URLs.
	t.Parallel()
	for _, fn := range []string{
		"a",
		"abc",
		validTestFn("short"),
		validTestFn("default"),
		strings.Repeat("z", 64), // hypothetically larger fn
	} {
		token := makeToken(testSignKey, fn)
		got, ok := parseToken(testSignKey, token)
		if !ok || got != fn {
			t.Errorf("parseToken round-trip failed for fn=%q: got=(%q, %v)", fn, got, ok)
		}
	}
}

func TestParseToken_RejectsForgeries(t *testing.T) {
	t.Parallel()
	fn := validTestFn("forgery")
	good := makeToken(testSignKey, fn)

	// Wrong key — handler must not trust whatever the attacker sends.
	if _, ok := parseToken([]byte("wrong-key"), good); ok {
		t.Error("parseToken accepted token signed under a different key")
	}

	// Tamper with the sig portion (last char).
	tampered := good[:len(good)-1] + "0"
	if tampered == good {
		tampered = good[:len(good)-1] + "1"
	}
	if _, ok := parseToken(testSignKey, tampered); ok {
		t.Errorf("parseToken accepted tampered sig: %q", tampered)
	}

	// Tamper with the fn portion (the sig no longer matches what we'd
	// compute over the rewritten fn).
	swapFn := "b" + good[1:]
	if swapFn == good {
		swapFn = "c" + good[1:]
	}
	if _, ok := parseToken(testSignKey, swapFn); ok {
		t.Errorf("parseToken accepted token with rewritten fn: %q", swapFn)
	}

	// Structural failures: missing separator, empty fn/sig, etc.
	for _, bad := range []string{
		"",
		"short",
		"no-separator-but-also-clearly-not-a-real-token",
		tokenSep + signFilename(testSignKey, ""), // empty fn
		fn + tokenSep,                            // empty sig
		fn,                                       // sig missing entirely
		strings.Repeat("a", fnLen+1+sigLen),      // right total length, no separator at all
	} {
		if _, ok := parseToken(testSignKey, bad); ok {
			t.Errorf("parseToken accepted bad token %q", bad)
		}
	}
}

func TestEscapeFilename(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want string }{
		{"file.txt", "file.txt"},
		{`say "hello".txt`, "say hello.txt"},
		{`"quoted"`, "quoted"},
		{"", ""},
		{`"`, ""},
	}
	for _, tc := range cases {
		if got := escapeFilename(tc.in); got != tc.want {
			t.Errorf("escapeFilename(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestFirstNonEmpty(t *testing.T) {
	t.Parallel()
	cases := []struct {
		vals []string
		want string
	}{
		{[]string{"a", "b"}, "a"},
		{[]string{"", "b"}, "b"},
		{[]string{"", ""}, ""},
		{[]string{}, ""},
		{[]string{"", "", "c"}, "c"},
	}
	for _, tc := range cases {
		if got := firstNonEmpty(tc.vals...); got != tc.want {
			t.Errorf("firstNonEmpty(%v) = %q, want %q", tc.vals, got, tc.want)
		}
	}
}

func TestJSONFail(t *testing.T) {
	t.Parallel()
	w := httptest.NewRecorder()
	jsonFail(w, "something broke", http.StatusBadRequest)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var resp failResp
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.OK {
		t.Error("ok should be false")
	}
	if resp.Error.Message != "something broke" {
		t.Errorf("message = %q", resp.Error.Message)
	}
}

func TestApp_AuthFallsThroughToCheckAuth(t *testing.T) {
	t.Parallel()
	// When App.checkAuth is nil, App.auth should call the package-level checkAuth.
	// Easiest probe: alpha-mode (empty auth header) -> authorized with project "alpha".
	app := &App{}
	res := app.auth(context.Background(), "", "", "")
	if !res.Authorized {
		t.Fatal("expected authorized via package checkAuth alpha mode")
	}
	if res.Project.ID != "alpha" {
		t.Errorf("project ID = %q, want alpha", res.Project.ID)
	}
}
