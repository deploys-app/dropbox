package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"gocloud.dev/blob"
	"gocloud.dev/blob/memblob"
)

// failBucket is a blobBucket whose NewWriter always returns an error.
type failBucket struct{ err error }

func (b *failBucket) NewWriter(_ context.Context, _ string, _ *blob.WriterOptions) (*blob.Writer, error) {
	return nil, b.err
}
func (b *failBucket) Delete(_ context.Context, _ string) error { return nil }

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

func unauthorized(_ context.Context, _, _, _ string) AuthResult {
	return AuthResult{}
}

func newTestApp(b blobBucket, authFn func(context.Context, string, string, string) AuthResult) *App {
	return &App{
		Bucket:    b,
		BaseURL:   "https://example.com/",
		checkAuth: authFn,
		execDB:    func(_ context.Context, _ string, _ ...any) error { return nil },
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
	app := newTestApp(newTestBucket(t), authorized)
	r := httptest.NewRequest(http.MethodGet, "/other", nil)
	w := httptest.NewRecorder()
	app.routes().ServeHTTP(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestUpload_EmptyBody(t *testing.T) {
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

func TestUpload_Success(t *testing.T) {
	bkt := newTestBucket(t)
	app := newTestApp(bkt, authorized)

	body := strings.NewReader("hello world")
	r := httptest.NewRequest(http.MethodPost, "/", body)
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
	if !strings.HasPrefix(resp.Result.DownloadURL, "https://example.com/1") {
		t.Errorf("downloadUrl = %q, want prefix with default TTL 1", resp.Result.DownloadURL)
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
}

func TestUpload_TTL(t *testing.T) {
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
			app := newTestApp(newTestBucket(t), authorized)

			url := "/"
			if tc.ttl != "" {
				url = "/?ttl=" + tc.ttl
			}
			body := strings.NewReader("data")
			r := httptest.NewRequest(http.MethodPost, url, body)
			r.ContentLength = 4
			w := httptest.NewRecorder()
			app.uploadHandler(w, r)

			var resp uploadResp
			json.NewDecoder(w.Body).Decode(&resp)
			if !resp.OK {
				t.Fatal("expected ok=true")
			}
			wantPrefix := "https://example.com/" + strconv.Itoa(tc.wantTTL)
			if !strings.HasPrefix(resp.Result.DownloadURL, wantPrefix) {
				t.Errorf("downloadUrl = %q, want prefix %q", resp.Result.DownloadURL, wantPrefix)
			}
		})
	}
}

func TestUpload_TTLFromHeader(t *testing.T) {
	app := newTestApp(newTestBucket(t), authorized)

	body := strings.NewReader("data")
	r := httptest.NewRequest(http.MethodPost, "/", body)
	r.ContentLength = 4
	r.Header.Set("param-ttl", "5")
	w := httptest.NewRecorder()
	app.uploadHandler(w, r)

	var resp uploadResp
	json.NewDecoder(w.Body).Decode(&resp)
	if !strings.HasPrefix(resp.Result.DownloadURL, "https://example.com/5") {
		t.Errorf("downloadUrl = %q, want TTL 5 from header", resp.Result.DownloadURL)
	}
}

func TestUpload_QueryParamOverridesHeader(t *testing.T) {
	app := newTestApp(newTestBucket(t), authorized)

	body := strings.NewReader("data")
	r := httptest.NewRequest(http.MethodPost, "/?ttl=6", body)
	r.ContentLength = 4
	r.Header.Set("param-ttl", "2")
	w := httptest.NewRecorder()
	app.uploadHandler(w, r)

	var resp uploadResp
	json.NewDecoder(w.Body).Decode(&resp)
	if !strings.HasPrefix(resp.Result.DownloadURL, "https://example.com/6") {
		t.Errorf("downloadUrl = %q, query param should take precedence over header", resp.Result.DownloadURL)
	}
}

func TestUpload_FilenameFromQuery(t *testing.T) {
	app := newTestApp(newTestBucket(t), authorized)

	body := strings.NewReader("data")
	r := httptest.NewRequest(http.MethodPost, "/?filename=report.pdf", body)
	r.ContentLength = 4
	w := httptest.NewRecorder()
	app.uploadHandler(w, r)

	var resp uploadResp
	json.NewDecoder(w.Body).Decode(&resp)
	if !resp.OK {
		t.Error("expected ok=true")
	}
}

func TestUpload_FilenameFromHeader(t *testing.T) {
	app := newTestApp(newTestBucket(t), authorized)

	body := strings.NewReader("data")
	r := httptest.NewRequest(http.MethodPost, "/", body)
	r.ContentLength = 4
	r.Header.Set("param-filename", "report.pdf")
	w := httptest.NewRecorder()
	app.uploadHandler(w, r)

	var resp uploadResp
	json.NewDecoder(w.Body).Decode(&resp)
	if !resp.OK {
		t.Error("expected ok=true")
	}
}

func TestUpload_Unauthorized(t *testing.T) {
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

func TestUpload_BucketError(t *testing.T) {
	app := newTestApp(&failBucket{err: errors.New("storage unavailable")}, authorized)

	body := strings.NewReader("data")
	r := httptest.NewRequest(http.MethodPost, "/", body)
	r.ContentLength = 4
	w := httptest.NewRecorder()
	app.uploadHandler(w, r)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
	var resp failResp
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.OK {
		t.Error("expected ok=false on bucket error")
	}
}

func TestGenerateFilename(t *testing.T) {
	a, b := generateFilename(), generateFilename()
	if a == b {
		t.Error("expected unique filenames")
	}
	for _, c := range a {
		if c == '+' || c == '/' || c == '=' {
			t.Errorf("filename contains non-URL-safe char %q: %s", c, a)
		}
	}
}

func TestEscapeFilename(t *testing.T) {
	cases := []struct{ in, want string }{
		{"file.txt", "file.txt"},
		{`say "hello".txt`, "say hello.txt"},
		{`"quoted"`, "quoted"},
	}
	for _, tc := range cases {
		if got := escapeFilename(tc.in); got != tc.want {
			t.Errorf("escapeFilename(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestFirstNonEmpty(t *testing.T) {
	cases := []struct {
		vals []string
		want string
	}{
		{[]string{"a", "b"}, "a"},
		{[]string{"", "b"}, "b"},
		{[]string{"", ""}, ""},
		{[]string{}, ""},
	}
	for _, tc := range cases {
		if got := firstNonEmpty(tc.vals...); got != tc.want {
			t.Errorf("firstNonEmpty(%v) = %q, want %q", tc.vals, got, tc.want)
		}
	}
}
