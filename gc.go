package main

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/acoshift/pgsql"
	"github.com/acoshift/pgsql/pgctx"
	"gocloud.dev/gcerrors"
)

func (a *App) gcHandler(w http.ResponseWriter, r *http.Request) {
	if a.InternalSecret != "" && r.Header.Get("Authorization") != "Bearer "+a.InternalSecret {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if err := a.runGC(r.Context()); err != nil {
		slog.Error("gc", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) runGC(ctx context.Context) error {
	var fns []string
	err := pgctx.Iter(ctx, func(scan pgsql.Scanner) error {
		var fn string
		if err := scan(&fn); err != nil {
			return err
		}
		fns = append(fns, fn)
		return nil
	}, `
		SELECT fn FROM files
		WHERE created_at + ttl * interval '1 day' < now()
	`)
	if err != nil {
		return err
	}

	slog.Info("gc: found expired files", "count", len(fns))

	for _, fn := range fns {
		if err := a.Bucket.Delete(ctx, fn); err != nil && gcerrors.Code(err) != gcerrors.NotFound {
			slog.Error("gc: delete from storage", "fn", fn, "error", err)
			continue
		}
		if _, err := pgctx.Exec(ctx, `DELETE FROM files WHERE fn = $1`, fn); err != nil {
			slog.Error("gc: delete from db", "fn", fn, "error", err)
		}
	}
	return nil
}
