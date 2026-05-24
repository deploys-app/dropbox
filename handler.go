package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
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
	checkAuth      func(ctx context.Context, auth, project, projectID string) AuthResult
}

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("Deploys.app Dropbox Service"))
	})
	mux.HandleFunc("POST /{$}", a.uploadHandler)
	mux.HandleFunc("GET /files/{fn}", a.fileHandler)
	mux.HandleFunc("GET /_cdn/{fn}", a.cdnFileHandler)
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
		INSERT INTO files (fn, project_id, size, filename, ttl, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, fn, authResult.Project.ID, n, filename, ttlDays, expiresAt); err != nil {
		slog.Error("insert file metadata", "error", err)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"ok": true,
		"result": map[string]any{
			"downloadUrl": a.BaseURL + fn,
			"expiresAt":   expiresAt.Format(time.RFC3339),
		},
	})
}

func generateFilename() string {
	b := make([]byte, 64)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
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
