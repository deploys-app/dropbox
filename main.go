package main

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"time"

	"github.com/acoshift/configfile"
	"github.com/acoshift/pgsql/pgctx"
	_ "github.com/lib/pq"
	"github.com/moonrhythm/cachestore"
	"github.com/moonrhythm/parapet"
	"github.com/moonrhythm/parapet/pkg/healthz"
	"github.com/moonrhythm/parapet/pkg/logger"
	"gocloud.dev/blob"
	_ "gocloud.dev/blob/gcsblob"
)

var config = configfile.NewEnvReader()

var logLevel slog.LevelVar

func main() {
	ctx := context.Background()

	if l := config.String("log_level"); l != "" {
		if err := logLevel.UnmarshalText([]byte(l)); err != nil {
			slog.Error("invalid log_level", "value", l)
		}
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: &logLevel,
	})))

	go cachestore.RunGCInterval(ctx, time.Hour)

	db, err := sql.Open("postgres", config.MustString("db_url"))
	if err != nil {
		slog.Error("open database", "error", err)
		os.Exit(1)
	}
	defer db.Close()
	db.SetMaxIdleConns(10)
	db.SetMaxOpenConns(10)
	db.SetConnMaxLifetime(10 * time.Minute)

	bkt, err := blob.OpenBucket(ctx, "gs://"+config.MustString("bucket_name"))
	if err != nil {
		slog.Error("open bucket", "error", err)
		os.Exit(1)
	}
	defer bkt.Close()

	app := &App{
		Bucket:         bkt,
		BaseURL:        config.StringDefault("base_url", "https://dropbox.deploys.app/files/"),
		InternalSecret: config.String("internal_secret"),
	}

	port := config.StringDefault("PORT", "8080")
	slog.Info("start dropbox", "addr", ":"+port)

	srv := parapet.NewBackend()
	srv.Addr = ":" + port
	srv.Use(healthz.New())
	srv.Use(logger.Stdout())
	srv.UseFunc(pgctx.Middleware(db))
	srv.Handler = app.routes()

	if err := srv.ListenAndServe(); err != nil {
		slog.Error("listen and serve", "error", err)
		os.Exit(1)
	}
}
