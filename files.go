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

	downloadCount.WithLabelValues(projectID).Inc()
	n, _ := io.Copy(w, reader)
	egressBytes.WithLabelValues(projectID).Add(float64(n))
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
