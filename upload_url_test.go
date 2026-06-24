package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"moonrhythm/dropbox/tu"
)

type uploadURLResp struct {
	OK     bool `json:"ok"`
	Result struct {
		Method          string `json:"method"`
		UploadURL       string `json:"uploadUrl"`
		DownloadURL     string `json:"downloadUrl"`
		ContentType     string `json:"contentType"`
		MinSize         int64  `json:"minSize"`
		MaxSize         int64  `json:"maxSize"`
		TTL             int    `json:"ttl"`
		UploadExpiresAt string `json:"uploadExpiresAt"`
	} `json:"result"`
}

type putResp struct {
	OK     bool `json:"ok"`
	Result struct {
		DownloadURL string `json:"downloadUrl"`
		Size        int64  `json:"size"`
		ExpiresAt   string `json:"expiresAt"`
	} `json:"result"`
}

const testUploadPrefix = "https://example.com/uploads/"

// issueUploadURL drives POST /uploads and returns the parsed response.
func issueUploadURL(t *testing.T, app *App, db *tu.Context, req uploadURLRequest) uploadURLResp {
	t.Helper()
	body, _ := json.Marshal(req)
	r := httptest.NewRequest(http.MethodPost, "/uploads", strings.NewReader(string(body)))
	r = r.WithContext(db.Ctx())
	r.Header.Set("Authorization", "Bearer x")
	w := httptest.NewRecorder()
	app.uploadURLHandler(w, r)
	var resp uploadURLResp
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode issue response: %v (%s)", err, w.Body.String())
	}
	if !resp.OK {
		t.Fatalf("issue upload url failed: %s", w.Body.String())
	}
	return resp
}

// uploadTokenOf pulls the signed upload token out of an uploadUrl.
func uploadTokenOf(t *testing.T, uploadURL string) string {
	t.Helper()
	if !strings.HasPrefix(uploadURL, testUploadPrefix) {
		t.Fatalf("uploadUrl %q missing prefix %q", uploadURL, testUploadPrefix)
	}
	return strings.TrimPrefix(uploadURL, testUploadPrefix)
}

// doPut PUTs body to /uploads/{token} through the mux (so {token} resolves) and
// returns the recorder.
func doPut(t *testing.T, app *App, db *tu.Context, token, contentType, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPut, "/uploads/"+token, strings.NewReader(body))
	r = r.WithContext(db.Ctx())
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	w := httptest.NewRecorder()
	app.routes().ServeHTTP(w, r)
	return w
}

func TestUploadURL_Success(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	bkt := newTestBucket(t)
	app := newTestApp(bkt, authorized)

	resp := issueUploadURL(t, app, db, uploadURLRequest{
		Project:     "myproject",
		TTL:         3,
		Filename:    "report.pdf",
		ContentType: "application/pdf",
		MaxSize:     1000,
		Expires:     120,
	})

	if resp.Result.Method != http.MethodPut {
		t.Errorf("method = %q, want PUT", resp.Result.Method)
	}
	if !strings.HasPrefix(resp.Result.UploadURL, testUploadPrefix) {
		t.Errorf("uploadUrl = %q, want %s… (our service, not GCS)", resp.Result.UploadURL, testUploadPrefix)
	}
	if !strings.HasPrefix(resp.Result.DownloadURL, "https://example.com/") {
		t.Errorf("downloadUrl = %q", resp.Result.DownloadURL)
	}
	if resp.Result.ContentType != "application/pdf" {
		t.Errorf("contentType = %q, want application/pdf", resp.Result.ContentType)
	}
	if resp.Result.MinSize != 1 || resp.Result.MaxSize != 1000 {
		t.Errorf("min/max = %d/%d, want 1/1000", resp.Result.MinSize, resp.Result.MaxSize)
	}
	if resp.Result.TTL != 3 {
		t.Errorf("ttl = %d, want 3", resp.Result.TTL)
	}
	uploadExp, _ := time.Parse(time.RFC3339, resp.Result.UploadExpiresAt)
	if d := time.Until(uploadExp); d < 60*time.Second || d > 180*time.Second {
		t.Errorf("uploadExpiresAt in %v, want ~120s", d)
	}

	// The token decodes to exactly the requested grant.
	g, ok := parseUploadToken(testSignKey, uploadTokenOf(t, resp.Result.UploadURL))
	if !ok {
		t.Fatal("uploadUrl token does not verify")
	}
	if g.ProjectID != "proj-1" || g.MinSize != 1 || g.MaxSize != 1000 || g.ContentType != "application/pdf" || g.TTL != 3 || g.Filename != "report.pdf" {
		t.Errorf("grant = %+v, unexpected", g)
	}
	// The download token in the response is the one for the granted fn.
	if want := "https://example.com/" + makeToken(testSignKey, g.FN); resp.Result.DownloadURL != want {
		t.Errorf("downloadUrl = %q, want %q", resp.Result.DownloadURL, want)
	}

	// Create writes nothing yet — no row, no object.
	if n := db.CountFiles(t); n != 0 {
		t.Errorf("db files = %d, want 0 (create is stateless)", n)
	}
	if n := countObjects(t, bkt); n != 0 {
		t.Errorf("bucket objects = %d, want 0 (create is stateless)", n)
	}
}

func TestUploadURL_Defaults(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	app := newTestApp(newTestBucket(t), authorized)

	r := httptest.NewRequest(http.MethodPost, "/uploads", nil)
	r = r.WithContext(db.Ctx())
	w := httptest.NewRecorder()
	app.uploadURLHandler(w, r)

	var resp uploadURLResp
	json.NewDecoder(w.Body).Decode(&resp)
	if !resp.OK {
		t.Fatal("expected ok=true")
	}
	if resp.Result.MinSize != 1 {
		t.Errorf("minSize = %d, want 1", resp.Result.MinSize)
	}
	if resp.Result.MaxSize != defaultMaxUploadSize {
		t.Errorf("maxSize = %d, want default cap %d", resp.Result.MaxSize, defaultMaxUploadSize)
	}
	if resp.Result.TTL != 1 {
		t.Errorf("ttl = %d, want 1", resp.Result.TTL)
	}
	if resp.Result.ContentType != "" {
		t.Errorf("contentType = %q, want empty when not requested", resp.Result.ContentType)
	}
	g, _ := parseUploadToken(testSignKey, uploadTokenOf(t, resp.Result.UploadURL))
	if want := time.Now().Add(defaultUploadURLExpiry).Unix(); g.Expiry < want-5 || g.Expiry > want+5 {
		t.Errorf("grant expiry = %d, want ~now+%v", g.Expiry, defaultUploadURLExpiry)
	}
}

func TestUploadURL_MaxSizeClampedToConfiguredCap(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	app := newTestApp(newTestBucket(t), authorized)
	app.MaxUploadSize = 500

	resp := issueUploadURL(t, app, db, uploadURLRequest{Project: "p", MaxSize: 1 << 40})
	if resp.Result.MaxSize != 500 {
		t.Errorf("maxSize = %d, want clamped to 500", resp.Result.MaxSize)
	}
	g, _ := parseUploadToken(testSignKey, uploadTokenOf(t, resp.Result.UploadURL))
	if g.MaxSize != 500 {
		t.Errorf("grant maxSize = %d, want 500", g.MaxSize)
	}
}

func TestUploadURL_MinGreaterThanMax(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	app := newTestApp(newTestBucket(t), authorized)

	body, _ := json.Marshal(uploadURLRequest{Project: "p", MinSize: 100, MaxSize: 50})
	r := httptest.NewRequest(http.MethodPost, "/uploads", strings.NewReader(string(body)))
	r = r.WithContext(db.Ctx())
	w := httptest.NewRecorder()
	app.uploadURLHandler(w, r)

	var resp failResp
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.OK || resp.Error.Message != "minSize greater than maxSize" {
		t.Errorf("got ok=%v msg=%q, want minSize greater than maxSize", resp.OK, resp.Error.Message)
	}
}

func TestUploadURL_Unauthorized(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	app := newTestApp(newTestBucket(t), unauthorized)

	body, _ := json.Marshal(uploadURLRequest{Project: "p"})
	r := httptest.NewRequest(http.MethodPost, "/uploads", strings.NewReader(string(body)))
	r = r.WithContext(db.Ctx())
	r.Header.Set("Authorization", "Bearer bad")
	w := httptest.NewRecorder()
	app.uploadURLHandler(w, r)

	var resp failResp
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.OK || resp.Error.Message != "api: unauthorized" {
		t.Errorf("got ok=%v msg=%q, want api: unauthorized", resp.OK, resp.Error.Message)
	}
}

func TestUploadURL_NumericProjectRoutesToID(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	var gotProject, gotProjectID string
	authFn := func(_ context.Context, _, project, projectID string) AuthResult {
		gotProject, gotProjectID = project, projectID
		return AuthResult{Authorized: true, Project: Project{ID: "proj-1"}}
	}
	app := newTestApp(newTestBucket(t), authFn)

	body, _ := json.Marshal(uploadURLRequest{Project: "12345"})
	r := httptest.NewRequest(http.MethodPost, "/uploads", strings.NewReader(string(body)))
	r = r.WithContext(db.Ctx())
	w := httptest.NewRecorder()
	app.uploadURLHandler(w, r)

	if gotProjectID != "12345" || gotProject != "" {
		t.Errorf("auth got project=%q projectId=%q, want \"\"/12345", gotProject, gotProjectID)
	}
}

func TestUploadDirect_Success(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	bkt := newTestBucket(t)
	pid := "direct-ok-" + t.Name()
	authFn := func(_ context.Context, _, _, _ string) AuthResult {
		return AuthResult{Authorized: true, Project: Project{ID: pid}}
	}
	app := newTestApp(bkt, authFn)

	resp := issueUploadURL(t, app, db, uploadURLRequest{
		Project: "p", TTL: 2, Filename: "doc.pdf", ContentType: "application/pdf", MaxSize: 1000,
	})
	token := uploadTokenOf(t, resp.Result.UploadURL)

	w := doPut(t, app, db, token, "application/pdf", "hello world")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	var pr putResp
	json.NewDecoder(w.Body).Decode(&pr)
	if !pr.OK {
		t.Fatal("expected ok=true")
	}
	if pr.Result.Size != int64(len("hello world")) {
		t.Errorf("size = %d, want %d", pr.Result.Size, len("hello world"))
	}
	if pr.Result.DownloadURL != resp.Result.DownloadURL {
		t.Errorf("downloadUrl = %q, want %q", pr.Result.DownloadURL, resp.Result.DownloadURL)
	}
	exp, _ := time.Parse(time.RFC3339, pr.Result.ExpiresAt)
	if d := time.Until(exp); d < 47*time.Hour || d > 49*time.Hour {
		t.Errorf("expiresAt in %v, want ~48h (ttl 2)", d)
	}

	// One row with the real size, the download token, project, ttl, filename.
	fn := tokenToFn(t, resp.Result.DownloadURL)
	var (
		size      int64
		ttl       int
		fname     string
		projectID string
		token2    string
	)
	if err := db.DB.QueryRowContext(context.Background(),
		`SELECT size, ttl, filename, project_id, token FROM files WHERE fn=$1`, fn).
		Scan(&size, &ttl, &fname, &projectID, &token2); err != nil {
		t.Fatal(err)
	}
	if size != int64(len("hello world")) || ttl != 2 || fname != "doc.pdf" || projectID != pid {
		t.Errorf("row = size %d ttl %d filename %q project %q, unexpected", size, ttl, fname, projectID)
	}
	if want := strings.TrimPrefix(resp.Result.DownloadURL, "https://example.com/"); token2 != want {
		t.Errorf("stored token = %q, want %q", token2, want)
	}

	// Object stored with the right content type + disposition.
	attrs, err := bkt.Attributes(context.Background(), fn)
	if err != nil {
		t.Fatal(err)
	}
	if attrs.ContentType != "application/pdf" {
		t.Errorf("object content-type = %q, want application/pdf", attrs.ContentType)
	}
	if !strings.Contains(attrs.ContentDisposition, `filename="doc.pdf"`) {
		t.Errorf("object content-disposition = %q", attrs.ContentDisposition)
	}

	if got := testutil.ToFloat64(uploadCount.WithLabelValues(pid)); got != 1 {
		t.Errorf("upload_count = %v, want 1", got)
	}
	if got := testutil.ToFloat64(uploadBytes.WithLabelValues(pid)); got != float64(len("hello world")) {
		t.Errorf("upload_bytes = %v, want %d", got, len("hello world"))
	}
}

func TestUploadDirect_TooLargeStreamed(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	bkt := newTestBucket(t)
	app := newTestApp(bkt, authorized)

	resp := issueUploadURL(t, app, db, uploadURLRequest{Project: "p", MaxSize: 4})
	token := uploadTokenOf(t, resp.Result.UploadURL)

	// Body of 10 bytes against a 4-byte cap, with ContentLength forced to -1 so
	// the early Content-Length check is bypassed and the streaming guard fires.
	r := httptest.NewRequest(http.MethodPut, "/uploads/"+token, strings.NewReader("0123456789"))
	r = r.WithContext(db.Ctx())
	r.ContentLength = -1
	w := httptest.NewRecorder()
	app.routes().ServeHTTP(w, r)

	var fr failResp
	json.NewDecoder(w.Body).Decode(&fr)
	if fr.OK || fr.Error.Message != "file too large" {
		t.Errorf("got ok=%v msg=%q, want file too large", fr.OK, fr.Error.Message)
	}
	if n := db.CountFiles(t); n != 0 {
		t.Errorf("db files = %d, want 0", n)
	}
	if n := countObjects(t, bkt); n != 0 {
		t.Errorf("bucket objects = %d, want 0 (oversize upload deleted)", n)
	}
}

func TestUploadDirect_TooLargeByContentLength(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	bkt := newTestBucket(t)
	app := newTestApp(bkt, authorized)

	resp := issueUploadURL(t, app, db, uploadURLRequest{Project: "p", MaxSize: 4})
	token := uploadTokenOf(t, resp.Result.UploadURL)

	w := doPut(t, app, db, token, "", "0123456789") // ContentLength=10 > 4
	var fr failResp
	json.NewDecoder(w.Body).Decode(&fr)
	if fr.OK || fr.Error.Message != "file too large" {
		t.Errorf("got ok=%v msg=%q, want file too large", fr.OK, fr.Error.Message)
	}
	if n := countObjects(t, bkt); n != 0 {
		t.Errorf("bucket objects = %d, want 0 (rejected before write)", n)
	}
}

func TestUploadDirect_TooSmall(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	bkt := newTestBucket(t)
	app := newTestApp(bkt, authorized)

	resp := issueUploadURL(t, app, db, uploadURLRequest{Project: "p", MinSize: 5, MaxSize: 100})
	token := uploadTokenOf(t, resp.Result.UploadURL)

	w := doPut(t, app, db, token, "", "ab") // 2 bytes < min 5
	var fr failResp
	json.NewDecoder(w.Body).Decode(&fr)
	if fr.OK || fr.Error.Message != "file too small" {
		t.Errorf("got ok=%v msg=%q, want file too small", fr.OK, fr.Error.Message)
	}
	if n := countObjects(t, bkt); n != 0 {
		t.Errorf("bucket objects = %d, want 0", n)
	}
}

func TestUploadDirect_SizeBoundaries(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	bkt := newTestBucket(t)
	app := newTestApp(bkt, authorized)

	// PUT with ContentLength forced to -1 so the streaming guard (not the early
	// Content-Length shortcut) decides — this is the off-by-one-sensitive path.
	put := func(req uploadURLRequest, body string) failResp {
		resp := issueUploadURL(t, app, db, req)
		r := httptest.NewRequest(http.MethodPut, "/uploads/"+uploadTokenOf(t, resp.Result.UploadURL), strings.NewReader(body))
		r = r.WithContext(db.Ctx())
		r.ContentLength = -1
		w := httptest.NewRecorder()
		app.routes().ServeHTTP(w, r)
		var fr failResp
		json.NewDecoder(w.Body).Decode(&fr)
		return fr
	}

	// Exactly maxSize passes; maxSize+1 is rejected.
	if fr := put(uploadURLRequest{Project: "p", MaxSize: 8}, "12345678"); !fr.OK {
		t.Errorf("8 bytes vs max 8: want ok, got %q", fr.Error.Message)
	}
	if fr := put(uploadURLRequest{Project: "p", MaxSize: 8}, "123456789"); fr.OK || fr.Error.Message != "file too large" {
		t.Errorf("9 bytes vs max 8: got ok=%v msg=%q, want file too large", fr.OK, fr.Error.Message)
	}
	// Exactly minSize passes; minSize-1 is rejected.
	if fr := put(uploadURLRequest{Project: "p", MinSize: 3, MaxSize: 100}, "abc"); !fr.OK {
		t.Errorf("3 bytes vs min 3: want ok, got %q", fr.Error.Message)
	}
	if fr := put(uploadURLRequest{Project: "p", MinSize: 3, MaxSize: 100}, "ab"); fr.OK || fr.Error.Message != "file too small" {
		t.Errorf("2 bytes vs min 3: got ok=%v msg=%q, want file too small", fr.OK, fr.Error.Message)
	}
}

func TestUploadDirect_Empty(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	bkt := newTestBucket(t)
	app := newTestApp(bkt, authorized)

	resp := issueUploadURL(t, app, db, uploadURLRequest{Project: "p", MaxSize: 100})
	token := uploadTokenOf(t, resp.Result.UploadURL)

	w := doPut(t, app, db, token, "", "")
	var fr failResp
	json.NewDecoder(w.Body).Decode(&fr)
	if fr.OK || fr.Error.Message != "body empty" {
		t.Errorf("got ok=%v msg=%q, want body empty", fr.OK, fr.Error.Message)
	}
	if n := db.CountFiles(t); n != 0 {
		t.Errorf("db files = %d, want 0", n)
	}
}

func TestUploadDirect_ContentTypeMismatch(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	bkt := newTestBucket(t)
	app := newTestApp(bkt, authorized)

	resp := issueUploadURL(t, app, db, uploadURLRequest{Project: "p", ContentType: "application/pdf", MaxSize: 100})
	token := uploadTokenOf(t, resp.Result.UploadURL)

	w := doPut(t, app, db, token, "text/plain", "data")
	var fr failResp
	json.NewDecoder(w.Body).Decode(&fr)
	if fr.OK || fr.Error.Message != "content type mismatch" {
		t.Errorf("got ok=%v msg=%q, want content type mismatch", fr.OK, fr.Error.Message)
	}
	if n := countObjects(t, bkt); n != 0 {
		t.Errorf("bucket objects = %d, want 0 (rejected before write)", n)
	}
}

func TestUploadDirect_InvalidToken(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	app := newTestApp(newTestBucket(t), authorized)
	w := doPut(t, app, db, "not.a.valid.token", "", "data")
	var fr failResp
	json.NewDecoder(w.Body).Decode(&fr)
	if fr.OK || fr.Error.Message != "invalid upload token" {
		t.Errorf("got ok=%v msg=%q, want invalid upload token", fr.OK, fr.Error.Message)
	}
}

func TestUploadDirect_ExpiredToken(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	bkt := newTestBucket(t)
	app := newTestApp(bkt, authorized)

	// Hand-mint a grant that already expired.
	token, err := makeUploadToken(testSignKey, uploadGrant{
		FN: generateFilename(), ProjectID: "p", MinSize: 1, MaxSize: 100, TTL: 1,
		Expiry: time.Now().Add(-time.Minute).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	w := doPut(t, app, db, token, "", "data")
	var fr failResp
	json.NewDecoder(w.Body).Decode(&fr)
	if fr.OK || fr.Error.Message != "upload url expired" {
		t.Errorf("got ok=%v msg=%q, want upload url expired", fr.OK, fr.Error.Message)
	}
	if n := countObjects(t, bkt); n != 0 {
		t.Errorf("bucket objects = %d, want 0", n)
	}
}

func TestUploadDirect_ReplayUpserts(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	bkt := newTestBucket(t)
	app := newTestApp(bkt, authorized)

	resp := issueUploadURL(t, app, db, uploadURLRequest{Project: "p", MaxSize: 100})
	token := uploadTokenOf(t, resp.Result.UploadURL)
	fn := tokenToFn(t, resp.Result.DownloadURL)

	if w := doPut(t, app, db, token, "", "first"); w.Code != http.StatusOK {
		t.Fatalf("first PUT status %d", w.Code)
	}
	if w := doPut(t, app, db, token, "", "second-longer"); w.Code != http.StatusOK {
		t.Fatalf("replay PUT status %d", w.Code)
	}

	// Replay must not leave a duplicate row; size reflects the last write.
	if n := db.CountFiles(t); n != 1 {
		t.Errorf("db files = %d, want 1 (replay upserts, no duplicate)", n)
	}
	var size int64
	if err := db.DB.QueryRowContext(context.Background(),
		`SELECT size FROM files WHERE fn=$1`, fn).Scan(&size); err != nil {
		t.Fatal(err)
	}
	if size != int64(len("second-longer")) {
		t.Errorf("size = %d, want %d (last write wins)", size, len("second-longer"))
	}
}

func TestUploadToken_RoundTripAndTamper(t *testing.T) {
	t.Parallel()
	g := uploadGrant{FN: "abc", ProjectID: "p1", MinSize: 1, MaxSize: 9, ContentType: "x/y", TTL: 3, Filename: "f.bin", Expiry: 1234567890}
	token, err := makeUploadToken(testSignKey, g)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := parseUploadToken(testSignKey, token)
	if !ok || got != g {
		t.Fatalf("round-trip = (%+v, %v), want (%+v, true)", got, ok, g)
	}
	// Wrong key.
	if _, ok := parseUploadToken([]byte("other-key"), token); ok {
		t.Error("verified under a different key")
	}
	// Tampered signature.
	if _, ok := parseUploadToken(testSignKey, token[:len(token)-1]+"0"); ok {
		t.Error("verified a tampered tag")
	}
	// Tampered payload (flip first char).
	bad := "X" + token[1:]
	if _, ok := parseUploadToken(testSignKey, bad); ok {
		t.Error("verified a tampered payload")
	}
	// Structural garbage.
	for _, b := range []string{"", "noseparator", ".", "a.", ".b"} {
		if _, ok := parseUploadToken(testSignKey, b); ok {
			t.Errorf("verified malformed token %q", b)
		}
	}
}

func TestUploadURL_DerivesServiceRoot(t *testing.T) {
	t.Parallel()
	app := &App{BaseURL: "https://dropbox.deploys.app/files/"}
	if got := app.uploadURL("tok"); got != "https://dropbox.deploys.app/uploads/tok" {
		t.Errorf("uploadURL = %q", got)
	}
}
