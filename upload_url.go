package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/acoshift/pgsql/pgctx"
	"github.com/moonrhythm/cachestore"
	"gocloud.dev/blob"
	"gocloud.dev/gcerrors"
)

// Signed-upload URLs let a caller hand a credential-free, time-limited URL to
// someone else (a browser, a model-built tool, a CI job) who then PUTs a file
// directly to this service — no deploys.app credential, and the request never
// goes through the caller's own backend.
//
// The flow is two steps:
//
//  1. POST /uploads — authorize (dropbox.upload), then mint a signed upload
//     token that self-describes the upload: the target fn, the project it bills
//     to, and the limits (min/max size, content type, ttl, expiry), all HMAC'd
//     under SignKey. Nothing is written to the DB or bucket yet. Returns the
//     uploadUrl (this service's /uploads/{token}) and the eventual downloadUrl.
//  2. PUT /uploads/{token} — verify the token (HMAC + expiry), enforce its limits
//     while streaming the body to GCS, then record the files row with the real
//     size in one shot. No auth header is needed: a valid token is the
//     capability. Because the bytes pass through this service it learns the size
//     directly, so there is no separate commit step and storage billing
//     (files.size) is exact immediately. An abandoned token writes nothing, so
//     there is no pending row to garbage-collect.
//
// This signs with our own HMAC key rather than a GCS signed URL, so it needs no
// IAM signBlob grant; the trade-off is that upload bytes transit this service
// (the same as the direct POST / handler).
const (
	// defaultUploadURLExpiry is how long an upload URL stays valid when the
	// caller doesn't ask for a specific window.
	defaultUploadURLExpiry = 15 * time.Minute
	// minUploadURLExpiry / maxUploadURLExpiry bound the requested window.
	minUploadURLExpiry = 1 * time.Second
	maxUploadURLExpiry = 1 * time.Hour
	// defaultMaxUploadSize caps an upload when App.MaxUploadSize is unset and the
	// caller doesn't pin a smaller maxSize.
	defaultMaxUploadSize = int64(5) << 30 // 5 GiB

	// uploadTokenSep separates the base64url payload from its hex HMAC tag.
	// Neither side can contain it (base64url is [A-Za-z0-9_-]; the tag is hex),
	// so a structural split is unambiguous.
	uploadTokenSep = "."
	// uploadSigLen is the hex length of the truncated HMAC tag (128-bit).
	uploadSigLen = 32
	// uploadTokenDomain domain-separates the upload-grant HMAC from the download
	// token HMAC (both keyed by SignKey) so neither can be repurposed as the
	// other.
	uploadTokenDomain = "dropbox-upload-grant\n"
)

// uploadGrant is the signed, self-describing capability carried in an upload
// token. Short JSON keys keep the token compact.
type uploadGrant struct {
	FN          string `json:"fn"`           // bucket object key + files.fn
	ProjectID   string `json:"p"`            // project the upload bills to
	MinSize     int64  `json:"mn"`           // inclusive lower bound, bytes (>= 1)
	MaxSize     int64  `json:"mx"`           // inclusive upper bound, bytes
	ContentType string `json:"ct,omitempty"` // required Content-Type when set
	TTL         int    `json:"t"`            // download lifetime, days
	Filename    string `json:"fl,omitempty"` // Content-Disposition filename
	Expiry      int64  `json:"x"`            // upload-URL expiry, unix seconds
}

// uploadSig returns the hex HMAC tag for a token payload.
func uploadSig(key []byte, payload string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(uploadTokenDomain))
	mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil)[:uploadSigLen/2])
}

// makeUploadToken encodes and signs a grant into a URL-safe token.
func makeUploadToken(key []byte, g uploadGrant) (string, error) {
	raw, err := json.Marshal(g)
	if err != nil {
		return "", err
	}
	payload := base64.RawURLEncoding.EncodeToString(raw)
	return payload + uploadTokenSep + uploadSig(key, payload), nil
}

// parseUploadToken verifies a token's HMAC (constant time) and returns the
// decoded grant. Expiry is the caller's responsibility to check. Any structural
// or signature failure returns (_, false) before any DB or bucket work — the
// same CPU-only shield the download path uses.
func parseUploadToken(key []byte, token string) (uploadGrant, bool) {
	i := strings.LastIndexByte(token, uploadTokenSep[0])
	if i <= 0 || i == len(token)-1 {
		return uploadGrant{}, false
	}
	payload, sig := token[:i], token[i+1:]
	if !hmac.Equal([]byte(sig), []byte(uploadSig(key, payload))) {
		return uploadGrant{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return uploadGrant{}, false
	}
	var g uploadGrant
	if err := json.Unmarshal(raw, &g); err != nil {
		return uploadGrant{}, false
	}
	return g, true
}

// uploadURLRequest is the JSON body of POST /uploads.
type uploadURLRequest struct {
	Project     string `json:"project"`     // project sid or numeric id
	ProjectID   string `json:"projectId"`   // numeric id (same as a numeric project)
	TTL         int    `json:"ttl"`         // file download lifetime, 1-7 days (default 1)
	Filename    string `json:"filename"`    // optional Content-Disposition on download
	ContentType string `json:"contentType"` // optional; the PUT must send this exactly
	MinSize     int64  `json:"minSize"`     // optional lower bound in bytes (>= 1)
	MaxSize     int64  `json:"maxSize"`     // optional upper bound in bytes (clamped to the cap)
	Expires     int    `json:"expires"`     // optional upload-URL validity in seconds
}

func (a *App) uploadURLHandler(w http.ResponseWriter, r *http.Request) {
	var req uploadURLRequest
	if r.Body != nil {
		// The body is a small JSON object; cap it defensively. An empty body is
		// fine — every field has a default.
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			jsonFail(w, "invalid request body", http.StatusOK)
			return
		}
	}

	auth := r.Header.Get("Authorization")
	project, projectID := req.Project, req.ProjectID
	// Same sid-vs-numeric-id routing as the direct upload handler.
	if projectID == "" && isAllDigits(project) {
		projectID, project = project, ""
	}

	authResult := a.auth(r.Context(), auth, project, projectID)
	if !authResult.Authorized {
		jsonFail(w, "api: unauthorized", http.StatusOK)
		return
	}

	ttlDays := req.TTL
	if ttlDays < 1 || ttlDays > 7 {
		ttlDays = 1
	}

	// Size bounds, enforced at upload time. maxSize is clamped to the service
	// cap; minSize never drops below 1 so an upload URL can't accept 0 bytes.
	maxCap := a.MaxUploadSize
	if maxCap <= 0 {
		maxCap = defaultMaxUploadSize
	}
	maxSize := req.MaxSize
	if maxSize <= 0 || maxSize > maxCap {
		maxSize = maxCap
	}
	minSize := req.MinSize
	if minSize < 1 {
		minSize = 1
	}
	if minSize > maxSize {
		jsonFail(w, "minSize greater than maxSize", http.StatusOK)
		return
	}

	expiry := time.Duration(req.Expires) * time.Second
	if expiry <= 0 {
		expiry = defaultUploadURLExpiry
	}
	expiry = min(max(expiry, minUploadURLExpiry), maxUploadURLExpiry)

	now := time.Now().UTC()
	uploadExpiresAt := now.Add(expiry)

	fn := generateFilename()
	// The download token is deterministic from fn, so the caller learns the
	// eventual download URL now; the upload token carries the signed grant.
	downloadToken := makeToken(a.SignKey, fn)
	uploadToken, err := makeUploadToken(a.SignKey, uploadGrant{
		FN:          fn,
		ProjectID:   authResult.Project.ID,
		MinSize:     minSize,
		MaxSize:     maxSize,
		ContentType: req.ContentType,
		TTL:         ttlDays,
		Filename:    req.Filename,
		Expiry:      uploadExpiresAt.Unix(),
	})
	if err != nil {
		slog.Error("make upload token", "error", err)
		jsonFail(w, "failed to create upload url", http.StatusInternalServerError)
		return
	}

	result := map[string]any{
		"method":          http.MethodPut,
		"uploadUrl":       a.uploadURL(uploadToken),
		"downloadUrl":     a.BaseURL + downloadToken,
		"minSize":         minSize,
		"maxSize":         maxSize,
		"ttl":             ttlDays,
		"uploadExpiresAt": uploadExpiresAt.Format(time.RFC3339),
	}
	// When a content type is pinned, the PUT must send it verbatim — surface it
	// so the caller knows which header to set.
	if req.ContentType != "" {
		result["contentType"] = req.ContentType
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
}

// serviceRoot derives the service base ("https://dropbox.deploys.app/") from
// BaseURL, the download prefix that ends in "files/". If BaseURL doesn't follow
// that documented shape, fall back to its scheme+host so we still emit a valid
// absolute URL rather than silently doubling a path segment.
func (a *App) serviceRoot() string {
	if root, ok := strings.CutSuffix(a.BaseURL, "files/"); ok {
		return root
	}
	if u, err := url.Parse(a.BaseURL); err == nil && u.Scheme != "" && u.Host != "" {
		return u.Scheme + "://" + u.Host + "/"
	}
	return a.BaseURL
}

func (a *App) uploadURL(uploadToken string) string {
	return a.serviceRoot() + "uploads/" + uploadToken
}

func (a *App) uploadDirectHandler(w http.ResponseWriter, r *http.Request) {
	g, ok := parseUploadToken(a.SignKey, r.PathValue("token"))
	if !ok {
		jsonFail(w, "invalid upload token", http.StatusOK)
		return
	}
	if time.Now().Unix() >= g.Expiry {
		jsonFail(w, "upload url expired", http.StatusOK)
		return
	}
	if g.ContentType != "" && r.Header.Get("Content-Type") != g.ContentType {
		jsonFail(w, "content type mismatch", http.StatusOK)
		return
	}
	if r.Body == nil {
		jsonFail(w, "body empty", http.StatusOK)
		return
	}
	// Reject early when the declared length already exceeds the cap; the
	// streaming guard below still covers a lying or absent Content-Length.
	if r.ContentLength > g.MaxSize {
		jsonFail(w, "file too large", http.StatusOK)
		return
	}

	opts := &blob.WriterOptions{CacheControl: "public, max-age=86400"}
	if g.ContentType != "" {
		opts.ContentType = g.ContentType
	}
	if g.Filename != "" {
		opts.ContentDisposition = fmt.Sprintf(`attachment; filename="%s"`, escapeFilename(g.Filename))
	}

	bw, err := a.Bucket.NewWriter(r.Context(), g.FN, opts)
	if err != nil {
		slog.Error("upload file", "error", err)
		jsonFail(w, "failed to upload", http.StatusInternalServerError)
		return
	}
	// Cap the read at MaxSize+1 so a body with a lying/absent Content-Length
	// can't stream past the signed limit; one extra byte tells us it overflowed.
	n, err := io.Copy(bw, io.LimitReader(r.Body, g.MaxSize+1))
	if err != nil {
		_ = bw.Close()
		a.deleteObject(r, g.FN)
		slog.Error("upload file", "error", err)
		jsonFail(w, "failed to upload", http.StatusInternalServerError)
		return
	}
	if err := bw.Close(); err != nil {
		a.deleteObject(r, g.FN)
		slog.Error("finalize upload", "error", err)
		jsonFail(w, "failed to upload", http.StatusInternalServerError)
		return
	}

	// Enforce the signed size bounds. Anything that violates them never becomes
	// a file — delete the object and write no row.
	if n > g.MaxSize {
		a.deleteObject(r, g.FN)
		jsonFail(w, "file too large", http.StatusOK)
		return
	}
	if n < g.MinSize {
		a.deleteObject(r, g.FN)
		msg := "file too small"
		if n == 0 {
			msg = "body empty"
		}
		jsonFail(w, msg, http.StatusOK)
		return
	}

	downloadToken := makeToken(a.SignKey, g.FN)
	expiresAt := time.Now().UTC().Add(time.Duration(g.TTL) * 24 * time.Hour)

	// Record the file, upserting on fn so a replayed PUT (same token reused
	// within its expiry) refreshes the size/expiry of the single row rather than
	// adding a duplicate that would double-count storage. fn is random per
	// upload, so this only ever conflicts with a replay; the unique index on fn
	// also turns the lookup into a point operation instead of a table scan.
	if _, err := pgctx.Exec(r.Context(), `
		INSERT INTO files (fn, project_id, size, filename, ttl, expires_at, token)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (fn) DO UPDATE SET
			size = excluded.size,
			expires_at = excluded.expires_at
	`, g.FN, g.ProjectID, n, g.Filename, g.TTL, expiresAt, downloadToken); err != nil {
		// Match the direct upload handler: the bytes are safely stored, so log
		// and still return success rather than lose the upload. The file serves
		// via the no-row fallback (no expiry enforced) until an operator notices.
		slog.Error("insert file metadata", "error", err)
	}

	// Clear any cached negative for this fn: a download attempted before the
	// upload landed would have cached BucketMissing, which would otherwise 404
	// the file for up to fileMetaCacheTTL. Best-effort and per-process (other
	// instances self-heal within that TTL).
	cachestore.Delete(fileCacheKey(g.FN))

	uploadCount.WithLabelValues(g.ProjectID).Inc()
	uploadBytes.WithLabelValues(g.ProjectID).Add(float64(n))

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"ok": true,
		"result": map[string]any{
			"downloadUrl": a.BaseURL + downloadToken,
			"size":        n,
			"expiresAt":   expiresAt.Format(time.RFC3339),
		},
	})
}

// deleteObject removes a just-written object, tolerating an already-absent one.
// It detaches from the request context: the most common reason we delete is a
// client disconnect mid-upload, which already canceled r.Context() — reusing it
// would fail the cleanup and orphan the object (GC is DB-driven and can't
// reclaim a bucket object that has no row).
func (a *App) deleteObject(r *http.Request, fn string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Second)
	defer cancel()
	if err := a.Bucket.Delete(ctx, fn); err != nil && gcerrors.Code(err) != gcerrors.NotFound {
		slog.Error("delete rejected upload", "fn", fn, "error", err)
	}
}
