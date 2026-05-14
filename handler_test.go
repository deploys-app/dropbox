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

// authorizedWith returns an auth function that always reports the supplied projectID.
// Used by parallel tests so each test scopes its DB rows to a unique project.
func authorizedWith(projectID string) func(context.Context, string, string, string) AuthResult {
	return func(_ context.Context, _, _, _ string) AuthResult {
		return AuthResult{Authorized: true, Project: Project{ID: projectID, Project: "test"}}
	}
}

func unauthorized(_ context.Context, _, _, _ string) AuthResult {
	return AuthResult{}
}

func newTestApp(bkt *blob.Bucket, authFn func(context.Context, string, string, string) AuthResult) *App {
	return &App{
		Bucket:    bkt,
		BaseURL:   "https://example.com/",
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

func TestUpload_Success(t *testing.T) {
	t.Parallel()
	projectID := "proj-" + t.Name()
	bkt := newTestBucket(t)
	app := newTestApp(bkt, authorizedWith(projectID))

	body := strings.NewReader("hello world")
	r := httptest.NewRequest(http.MethodPost, "/", body)
	r = r.WithContext(testDB.Ctx())
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
	if n := testDB.CountFilesByProject(t, projectID); n != 1 {
		t.Errorf("db files for project %q = %d, want 1", projectID, n)
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
			projectID := "proj-" + t.Name()
			app := newTestApp(newTestBucket(t), authorizedWith(projectID))

			url := "/"
			if tc.ttl != "" {
				url = "/?ttl=" + tc.ttl
			}
			body := strings.NewReader("data")
			r := httptest.NewRequest(http.MethodPost, url, body)
			r = r.WithContext(testDB.Ctx())
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
	app := newTestApp(newTestBucket(t), authorizedWith("proj-"+t.Name()))

	body := strings.NewReader("data")
	r := httptest.NewRequest(http.MethodPost, "/", body)
	r = r.WithContext(testDB.Ctx())
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
	app := newTestApp(newTestBucket(t), authorizedWith("proj-"+t.Name()))

	body := strings.NewReader("data")
	r := httptest.NewRequest(http.MethodPost, "/?ttl=6", body)
	r = r.WithContext(testDB.Ctx())
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
	bkt := newTestBucket(t)
	app := newTestApp(bkt, authorizedWith("proj-"+t.Name()))

	body := strings.NewReader("data")
	r := httptest.NewRequest(http.MethodPost, "/?filename=report.pdf", body)
	r = r.WithContext(testDB.Ctx())
	r.ContentLength = 4
	w := httptest.NewRecorder()
	app.uploadHandler(w, r)

	var resp uploadResp
	json.NewDecoder(w.Body).Decode(&resp)
	if !resp.OK {
		t.Fatal("expected ok=true")
	}

	fn := strings.TrimPrefix(resp.Result.DownloadURL, "https://example.com/")
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
	bkt := newTestBucket(t)
	app := newTestApp(bkt, authorizedWith("proj-"+t.Name()))

	body := strings.NewReader("data")
	r := httptest.NewRequest(http.MethodPost, "/", body)
	r = r.WithContext(testDB.Ctx())
	r.ContentLength = 4
	r.Header.Set("param-filename", "report.pdf")
	w := httptest.NewRecorder()
	app.uploadHandler(w, r)

	var resp uploadResp
	json.NewDecoder(w.Body).Decode(&resp)
	if !resp.OK {
		t.Fatal("expected ok=true")
	}

	fn := strings.TrimPrefix(resp.Result.DownloadURL, "https://example.com/")
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
	bkt := newTestBucket(t)
	app := newTestApp(bkt, authorizedWith("proj-"+t.Name()))

	body := strings.NewReader("data")
	r := httptest.NewRequest(http.MethodPost, `/?filename=say+"hi".txt`, body)
	r = r.WithContext(testDB.Ctx())
	r.ContentLength = 4
	w := httptest.NewRecorder()
	app.uploadHandler(w, r)

	var resp uploadResp
	json.NewDecoder(w.Body).Decode(&resp)
	if !resp.OK {
		t.Fatal("expected ok=true")
	}

	fn := strings.TrimPrefix(resp.Result.DownloadURL, "https://example.com/")
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
	bkt := newTestBucket(t)
	app := newTestApp(bkt, authorizedWith("proj-"+t.Name()))

	body := strings.NewReader("data")
	r := httptest.NewRequest(http.MethodPost, "/", body)
	r = r.WithContext(testDB.Ctx())
	r.ContentLength = 4
	w := httptest.NewRecorder()
	app.uploadHandler(w, r)

	var resp uploadResp
	json.NewDecoder(w.Body).Decode(&resp)
	fn := strings.TrimPrefix(resp.Result.DownloadURL, "https://example.com/")
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

	// Track what the auth function was called with.
	var gotProject, gotProjectID string
	authFn := func(_ context.Context, _, project, projectID string) AuthResult {
		gotProject, gotProjectID = project, projectID
		return AuthResult{Authorized: true, Project: Project{ID: "proj-" + t.Name()}}
	}
	app := newTestApp(newTestBucket(t), authFn)

	body := strings.NewReader("data")
	r := httptest.NewRequest(http.MethodPost, "/?projectId=pid-from-query", body)
	r = r.WithContext(testDB.Ctx())
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

func TestGenerateFilename(t *testing.T) {
	t.Parallel()
	a, b := generateFilename(), generateFilename()
	if a == b {
		t.Error("expected unique filenames")
	}
	if len(a) != 86 {
		t.Errorf("filename length = %d, want 86", len(a))
	}
	for _, c := range a {
		if c == '+' || c == '/' || c == '=' {
			t.Errorf("filename contains non-URL-safe char %q: %s", c, a)
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
