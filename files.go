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

const fileMetaCacheTTL = 60 * time.Second

// fileMeta is the file's row in the files table. Found is false when there
// is no row for the fn — that happens when the metadata insert failed
// during upload; we still serve the bucket bytes in that case rather than
// 404'ing, since we have no expiration to enforce against.
type fileMeta struct {
	ProjectID string
	ExpiresAt time.Time
	Found     bool
}

// Expired reports whether the file has a recorded expiry that is now in
// the past. Files without a DB row (Found == false) and files with a NULL
// expires_at are treated as non-expired — we have no basis to refuse them.
func (m fileMeta) Expired() bool {
	return m.Found && !m.ExpiresAt.IsZero() && m.ExpiresAt.Before(time.Now())
}

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

	meta := lookupFile(r.Context(), fn)
	if meta.Expired() {
		// Refuse before counting metrics or redirecting — the GC will
		// catch up and drop the object, but until then we don't want the
		// CDN to cache (or the caller to receive) bytes the user is no
		// longer entitled to.
		http.Error(w, "file expired", http.StatusGone)
		return
	}

	// When a CDN is configured and the caller is not in-cluster, record the
	// download as if we were serving the full file (the CDN will handle the
	// actual bytes) and redirect. Cache hits never come back to this origin,
	// so the bill is an over-approximation; that matches the registry
	// pattern and is what /_internal/calculate-dropbox-usages assumes.
	if a.CDNBaseURL != "" && !isInternalClient(r) {
		downloadCount.WithLabelValues(meta.ProjectID).Inc()
		egressBytes.WithLabelValues(meta.ProjectID).Add(float64(attrs.Size))
		http.Redirect(w, r, a.CDNBaseURL+fn, http.StatusTemporaryRedirect)
		return
	}

	a.streamFile(w, r, fn, attrs, meta.ProjectID)
}

// cdnFileHandler is the origin endpoint the CDN edge fetches from. It is
// unauthenticated — file URLs are 86-char crypto-random and only reachable
// if you know them — and skips the redirect/metrics so the CDN sees a
// plain streaming response. Expired files are refused here too so the CDN
// can't refresh its cache with bytes the user is no longer entitled to.
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

	if lookupFile(r.Context(), fn).Expired() {
		http.Error(w, "file expired", http.StatusGone)
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

// lookupFile fetches the file's project owner and expiration from the DB.
// Both fields are immutable once written, so the in-process cache only
// needs to be short enough to absorb bursts on the same fn — not to track
// any state that can change underneath us.
func lookupFile(ctx context.Context, fn string) fileMeta {
	cacheKey := "fn|" + fn
	if v, ok := cachestore.Get[fileMeta](cacheKey); ok {
		return v
	}

	var (
		m       fileMeta
		expires sql.NullTime
	)
	err := pgctx.QueryRow(ctx, `
		SELECT project_id, expires_at FROM files WHERE fn = $1
	`, fn).Scan(&m.ProjectID, &expires)
	switch {
	case err == nil:
		m.Found = true
		if expires.Valid {
			m.ExpiresAt = expires.Time
		}
	case errors.Is(err, sql.ErrNoRows):
		// Leave m zero-valued (Found=false). Don't cache-poison on
		// transient DB errors below, but a confirmed miss is fine.
	default:
		slog.Error("lookup file", "fn", fn, "error", err)
		return fileMeta{}
	}

	cachestore.Set(cacheKey, m, &cachestore.SetOptions{TTL: fileMetaCacheTTL})
	return m
}
