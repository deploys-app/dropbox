package main

import (
	"io"
	"net/http"
	"strconv"

	"gocloud.dev/gcerrors"
)

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

	io.Copy(w, reader)
}
