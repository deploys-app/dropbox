package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/acoshift/pgsql/pgctx"
	"gocloud.dev/blob"
)

type App struct {
	Bucket         *blob.Bucket
	BaseURL        string
	CDNBaseURL     string
	InternalSecret string
	SignKey        []byte
	checkAuth      func(ctx context.Context, auth, project, projectID string) AuthResult
}

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("Deploys.app Dropbox Service"))
	})
	mux.HandleFunc("POST /{$}", a.uploadHandler)
	mux.HandleFunc("GET /files/{token}", a.fileHandler)
	mux.HandleFunc("GET /_cdn/{token}", a.cdnFileHandler)
	mux.HandleFunc("POST /internal/gc", a.gcHandler)
	return mux
}

func (a *App) auth(ctx context.Context, auth, project, projectID string) AuthResult {
	if a.checkAuth != nil {
		return a.checkAuth(ctx, auth, project, projectID)
	}
	return checkAuth(ctx, auth, project, projectID)
}

func (a *App) uploadHandler(w http.ResponseWriter, r *http.Request) {
	auth := r.Header.Get("Authorization")
	project := firstNonEmpty(r.URL.Query().Get("project"), r.Header.Get("param-project"))
	projectID := firstNonEmpty(r.URL.Query().Get("projectId"), r.Header.Get("param-project-id"))

	authResult := a.auth(r.Context(), auth, project, projectID)
	if !authResult.Authorized {
		jsonFail(w, "api: unauthorized", http.StatusOK)
		return
	}

	ttlStr := firstNonEmpty(r.URL.Query().Get("ttl"), r.Header.Get("param-ttl"))
	ttlDays, _ := strconv.Atoi(ttlStr)
	if ttlDays < 1 || ttlDays > 7 {
		ttlDays = 1
	}

	filename := firstNonEmpty(r.URL.Query().Get("filename"), r.Header.Get("param-filename"))

	if r.Body == nil || r.ContentLength == 0 {
		jsonFail(w, "body empty", http.StatusOK)
		return
	}

	expiresAt := time.Now().UTC().Add(time.Duration(ttlDays) * 24 * time.Hour)
	fn := generateFilename()
	// The signed token is the public URL component. Persist it so the api's
	// dropbox.List can rebuild download URLs without holding SignKey.
	token := makeToken(a.SignKey, fn)

	opts := &blob.WriterOptions{
		CacheControl: "public, max-age=86400",
	}
	if filename != "" {
		opts.ContentDisposition = fmt.Sprintf(`attachment; filename="%s"`, escapeFilename(filename))
	}

	bw, err := a.Bucket.NewWriter(r.Context(), fn, opts)
	if err != nil {
		slog.Error("upload file", "error", err)
		jsonFail(w, "failed to upload", http.StatusInternalServerError)
		return
	}
	n, err := io.Copy(bw, r.Body)
	if err != nil {
		_ = bw.Close()
		slog.Error("upload file", "error", err)
		jsonFail(w, "failed to upload", http.StatusInternalServerError)
		return
	}
	if err := bw.Close(); err != nil {
		slog.Error("finalize upload", "error", err)
		jsonFail(w, "failed to upload", http.StatusInternalServerError)
		return
	}

	uploadCount.WithLabelValues(authResult.Project.ID).Inc()
	uploadBytes.WithLabelValues(authResult.Project.ID).Add(float64(n))

	if _, err := pgctx.Exec(r.Context(), `
		INSERT INTO files (fn, project_id, size, filename, ttl, expires_at, token)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, fn, authResult.Project.ID, n, filename, ttlDays, expiresAt, token); err != nil {
		slog.Error("insert file metadata", "error", err)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"ok": true,
		"result": map[string]any{
			"downloadUrl": a.BaseURL + token,
			"expiresAt":   expiresAt.Format(time.RFC3339),
		},
	})
}

// Filename + signature scheme.
//
// `fn` is what we store in the bucket and the DB — a random 24-char
// alphanumeric string (~143 bits of entropy, drawn from
// [0-9A-Za-z] with rejection sampling). No special chars, so URLs are
// clean and we don't need URL-safe-base64 tricks.
//
// `token` is what appears in the public URL: `fn` + "-" + 20-char
// lowercase-hex HMAC-SHA256 tag (80-bit) keyed by App.SignKey. The
// handlers check the tag before any DB or GCS work, so a flood of
// random `/files/{token}` requests by an attacker who doesn't know
// the key gets 404'd in CPU. The "-" separator means we can change
// `fnLen` later without invalidating old tokens — parseToken splits
// on it instead of relying on a fixed cut point. fn is alphanumeric
// and sig is hex, so neither side can contain a "-".
const (
	fnLen    = 24
	sigLen   = 20 // 80 bits of HMAC, hex-encoded
	tokenSep = "-"
)

const fnAlphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// generateFilename returns a fresh fnLen-char random alphanumeric fn.
// Uses rejection sampling so each char is uniformly drawn from
// fnAlphabet (avoiding the mod-bias you'd get from `b%62`).
func generateFilename() string {
	out := make([]byte, fnLen)
	buf := make([]byte, fnLen*2) // slack for rejection
	pos := 0
	for pos < fnLen {
		if _, err := rand.Read(buf); err != nil {
			panic(err) // crypto/rand failing is unrecoverable
		}
		for _, b := range buf {
			if pos >= fnLen {
				break
			}
			// 62 * 4 = 248 is the largest unbiased range under 256;
			// reject 248..255.
			if b < 248 {
				out[pos] = fnAlphabet[b%62]
				pos++
			}
		}
	}
	return string(out)
}

// signFilename returns the sigLen-char lowercase-hex HMAC tag for fn
// under key. Caller's responsibility to keep `key` secret.
func signFilename(key []byte, fn string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(fn))
	sum := mac.Sum(nil)
	return hex.EncodeToString(sum[:sigLen/2])
}

// makeToken builds the public URL token for fn: `fn + "-" + sig`.
func makeToken(key []byte, fn string) string {
	return fn + tokenSep + signFilename(key, fn)
}

// parseToken splits a public URL token on tokenSep, verifies the HMAC
// in constant time, and returns the fn on success. On any failure —
// missing separator, empty side, bad sig, anything — it returns
// (_, false) and the handler 404s without touching the DB or GCS.
// This is the primary DDoS shield against random `/files/{garbage}`
// floods. The split is structural (on the separator) rather than
// positional, so changing fnLen later won't invalidate old tokens
// still in circulation.
func parseToken(key []byte, token string) (string, bool) {
	idx := strings.IndexByte(token, tokenSep[0])
	if idx <= 0 || idx == len(token)-1 {
		return "", false
	}
	fn := token[:idx]
	sig := token[idx+1:]
	expected := signFilename(key, fn)
	if !hmac.Equal([]byte(sig), []byte(expected)) {
		return "", false
	}
	return fn, true
}

func escapeFilename(s string) string {
	return strings.ReplaceAll(s, `"`, "")
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// isInternalClient returns true when the request originates from a
// private/loopback/link-local IP per the X-Real-Ip header. Internal callers
// (in-cluster fetches) bypass the CDN redirect and read files directly.
func isInternalClient(r *http.Request) bool {
	ip := net.ParseIP(r.Header.Get("X-Real-Ip"))
	if ip == nil {
		return false
	}
	return ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast()
}

func jsonFail(w http.ResponseWriter, msg string, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"ok": false,
		"error": map[string]any{
			"message": msg,
		},
	})
}
