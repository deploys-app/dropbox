package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	"github.com/acoshift/pgsql/pgctx"
)

type bucket interface {
	upload(ctx context.Context, name, cacheControl, contentDisposition string, r io.Reader) error
	delete(ctx context.Context, name string) error
}

type gcsBucket struct {
	handle *storage.BucketHandle
}

func (b *gcsBucket) upload(ctx context.Context, name, cacheControl, contentDisposition string, r io.Reader) error {
	wc := b.handle.Object(name).NewWriter(ctx)
	wc.CacheControl = cacheControl
	if contentDisposition != "" {
		wc.ContentDisposition = contentDisposition
	}
	if _, err := io.Copy(wc, r); err != nil {
		_ = wc.Close()
		return err
	}
	return wc.Close()
}

func (b *gcsBucket) delete(ctx context.Context, name string) error {
	err := b.handle.Object(name).Delete(ctx)
	if errors.Is(err, storage.ErrObjectNotExist) {
		return nil
	}
	return err
}

type App struct {
	Bucket         bucket
	BaseURL        string
	InternalSecret string
	checkAuth      func(ctx context.Context, auth, project, projectID string) AuthResult
	execDB         func(ctx context.Context, query string, args ...any) error
}

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("Deploys.app Dropbox Service"))
	})
	mux.HandleFunc("POST /{$}", a.uploadHandler)
	mux.HandleFunc("POST /internal/gc", a.gcHandler)
	return mux
}

func (a *App) auth(ctx context.Context, auth, project, projectID string) AuthResult {
	if a.checkAuth != nil {
		return a.checkAuth(ctx, auth, project, projectID)
	}
	return checkAuth(ctx, auth, project, projectID)
}

func (a *App) dbExec(ctx context.Context, query string, args ...any) error {
	if a.execDB != nil {
		return a.execDB(ctx, query, args...)
	}
	_, err := pgctx.Exec(ctx, query, args...)
	return err
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
	fn := strconv.Itoa(ttlDays) + generateFilename()

	cacheControl := "public, max-age=86400"
	contentDisposition := ""
	if filename != "" {
		contentDisposition = fmt.Sprintf(`attachment; filename="%s"`, escapeFilename(filename))
	}

	if err := a.Bucket.upload(r.Context(), fn, cacheControl, contentDisposition, r.Body); err != nil {
		slog.Error("upload file", "error", err)
		jsonFail(w, "failed to upload", http.StatusInternalServerError)
		return
	}

	if err := a.dbExec(r.Context(), `
		INSERT INTO files (fn, project_id, size, filename, ttl)
		VALUES ($1, $2, $3, $4, $5)
	`, fn, authResult.Project.ID, r.ContentLength, filename, ttlDays); err != nil {
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
