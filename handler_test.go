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
	t.Cleanup(func() { testDB.DeleteFiles(t) })

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
	if n := testDB.CountFiles(t); n != 1 {
		t.Errorf("db files = %d, want 1", n)
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
			t.Cleanup(func() { testDB.DeleteFiles(t) })

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
	app := newTestApp(newTestBucket(t), authorized)
	t.Cleanup(func() { testDB.DeleteFiles(t) })

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
	app := newTestApp(newTestBucket(t), authorized)
	t.Cleanup(func() { testDB.DeleteFiles(t) })

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
	app := newTestApp(newTestBucket(t), authorized)
	t.Cleanup(func() { testDB.DeleteFiles(t) })

	body := strings.NewReader("data")
	r := httptest.NewRequest(http.MethodPost, "/?filename=report.pdf", body)
	r = r.WithContext(testDB.Ctx())
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
	t.Cleanup(func() { testDB.DeleteFiles(t) })

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
