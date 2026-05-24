package main

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/acoshift/pgsql/pgctx"
	"github.com/moonrhythm/cachestore"
	"gocloud.dev/blob"
	"gocloud.dev/gcerrors"
)

const fileProjectCacheTTL = 60 * time.Second

func (a *App) fileHandler(w http.ResponseWriter, r *http.Request) {
	fn := r.PathValue("fn")

	attrs, err := a.Bucket.Attributes(r.Context(), fn)
	if err != nil {
		if gcerrors.Code(err) == gcerrors.NotFound {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	projectID := lookupFileProject(r.Context(), fn)

	// When a CDN is configured and the caller is not in-cluster, record the
	// download as if we were serving the full file (the CDN will handle the
	// actual bytes) and redirect. Cache hits never come back to this origin,
	// so the bill is an over-approximation; that matches the registry
	// pattern and is what /_internal/calculate-dropbox-usages assumes.
	if a.CDNDomain != "" && !isInternalClient(r) {
		downloadCount.WithLabelValues(projectID).Inc()
		egressBytes.WithLabelValues(projectID).Add(float64(attrs.Size))
		http.Redirect(w, r, "https://"+a.CDNDomain+"/"+fn, http.StatusTemporaryRedirect)
		return
	}

	a.streamFile(w, r, fn, attrs, projectID)
}

// cdnFileHandler is the origin endpoint the CDN edge fetches from. It is
// unauthenticated — file URLs are 86-char crypto-random and only reachable
// if you know them — and skips the redirect/metrics so the CDN sees a
// plain streaming response.
func (a *App) cdnFileHandler(w http.ResponseWriter, r *http.Request) {
	fn := r.PathValue("fn")

	attrs, err := a.Bucket.Attributes(r.Context(), fn)
	if err != nil {
		if gcerrors.Code(err) == gcerrors.NotFound {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	a.streamFile(w, r, fn, attrs, "")
}

// streamFile copies the object body to w with the cached headers. When
// projectID is non-empty, download_count and egress_bytes are bumped by
// the actual transferred bytes. Used by both the direct-stream path
// (no CDN configured) and the unauthenticated _cdn origin path.
func (a *App) streamFile(w http.ResponseWriter, r *http.Request, fn string, attrs *blob.Attributes, projectID string) {
	reader, err := a.Bucket.NewReader(r.Context(), fn, nil)
	if err != nil {
		if gcerrors.Code(err) == gcerrors.NotFound {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	defer reader.Close()

	if attrs.CacheControl != "" {
		w.Header().Set("Cache-Control", attrs.CacheControl)
	}
	if attrs.ContentDisposition != "" {
		w.Header().Set("Content-Disposition", attrs.ContentDisposition)
	}
	if attrs.ContentType != "" {
		w.Header().Set("Content-Type", attrs.ContentType)
	}
	w.Header().Set("Content-Length", strconv.FormatInt(attrs.Size, 10))

	if projectID != "" {
		downloadCount.WithLabelValues(projectID).Inc()
	}
	n, _ := io.Copy(w, reader)
	if projectID != "" {
		egressBytes.WithLabelValues(projectID).Add(float64(n))
	}
}

func lookupFileProject(ctx context.Context, fn string) string {
	cacheKey := "fn|" + fn
	if v, ok := cachestore.Get[string](cacheKey); ok {
		return v
	}

	var projectID string
	err := pgctx.QueryRow(ctx, `
		SELECT project_id FROM files WHERE fn = $1
	`, fn).Scan(&projectID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		slog.Error("lookup file project", "fn", fn, "error", err)
		return ""
	}

	cachestore.Set(cacheKey, projectID, &cachestore.SetOptions{TTL: fileProjectCacheTTL})
	return projectID
}
