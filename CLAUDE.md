# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```bash
go build ./...          # Build
go run .                # Run locally
go mod tidy             # Sync dependencies
```

Docker image is built and pushed to GCR/Artifact Registry via `.github/workflows/build.yaml` on push to main.

## Architecture

Standard Go HTTP server (not Cloudflare Workers) serving as a temporary file upload service for deploys.app. Accepts `POST /`, stores files in Google Cloud Storage, records metadata in PostgreSQL, and returns a download URL with TTL-based expiration.

**Environment variables (required):**
- `db_url` — PostgreSQL connection string
- `bucket_name` — GCS bucket name

**Environment variables (optional):**
- `base_url` — download URL prefix (default: `https://dropbox.deploys-files.app/`)
- `api_endpoint` — deploys.app API base URL (default: `https://api.deploys.app`, override with internal address in production)
- `PORT` — listen port (default: `8080`)
- `log_level` — slog level (default: info)

**Request flow (`handler.go`):**
1. Parse `Authorization` header + `project`/`projectId` from query params or `param-*` headers (query params take precedence)
2. Authorize via `checkAuth()` in `auth.go`
3. Parse TTL (1–7 days, default 1) and optional filename the same way
4. Generate a crypto-random 86-char URL-safe base64 filename with TTL digit prepended (e.g., `1ABC…`)
5. Stream body to GCS with cache-control and optional content-disposition
6. Insert metadata into PostgreSQL via `pgctx.Exec`
7. Return JSON: `{"ok": true, "result": {"downloadUrl": "...", "expiresAt": "..."}}`

**Auth (`auth.go`):**
- No `Authorization` header → alpha mode, project ID hardcoded as `"alpha"` (TODO: remove)
- With token → POST to `https://api.deploys.app/me.authorized` for `dropbox.upload` permission, checking `authorized` + `billingAccount.active`
- Results cached in-process for 30 seconds via `cachestore`

**Key libraries (same pattern as `moonrhythm/registry`):**
- `parapet` — HTTP server with middleware chain (healthz, logger, pgctx)
- `pgctx` — context-aware PostgreSQL access (`pgctx.Exec`, middleware injects DB into context)
- `cachestore` — in-process TTL cache for auth results
- `configfile` — env-var config reader (`config.MustString`, `config.StringDefault`)

## Notes

- `schema.sql` targets PostgreSQL; `project_id` is `text` (the API returns string IDs)
- The download domain (`base_url`) differs from the upload domain — files are served from a separate host
