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
- `sign_key` — HMAC key for signing download tokens (`handler.go:signFilename`). Rotating it invalidates every outstanding URL — that's the right behavior if a key leaks.

**Environment variables (optional):**
- `base_url` — download URL prefix (default: `https://dropbox.deploys.app/files/`)
- `api_endpoint` — deploys.app API base URL (default: `https://api.deploys.app`, override with internal address in production)
- `cdn_base_url` — full URL prefix (including scheme and trailing slash, e.g. `https://cdn.example.com/`). When set, `GET /files/{token}` records the download metric (counted against `attrs.Size`) and 307-redirects to `{cdn_base_url}{token}`. The CDN edge is expected to fetch its origin at `https://dropbox.deploys.app/_cdn/{token}`, which streams the file (no auth, no metrics) after re-verifying the same HMAC. In-cluster callers (private/loopback/link-local `X-Real-Ip`) bypass the redirect and stream directly. Unset = original streaming behavior.
- `PORT` — listen port (default: `8080`)
- `log_level` — slog level (default: info)

**Download token scheme (`handler.go`):**
- The URL path component is a 44-char `token` = `fn` (24 chars random `[0-9A-Za-z]`, ~143 bits of entropy) concatenated with `sig` (20 hex chars of HMAC-SHA256 truncated to 80 bits, keyed by `sign_key`).
- `parseToken(SignKey, token)` runs first in both `fileHandler` and `cdnFileHandler` and 404s on any mismatch — DDoS attempts that don't know `sign_key` never reach the DB or GCS.
- `fn` is what we store in the bucket and the `files.fn` column. The full token only appears in URLs.

**Request flow (`handler.go`):**
1. Parse `Authorization` header + `project`/`projectId` from query params or `param-*` headers (query params take precedence)
2. Authorize via `checkAuth()` in `auth.go`
3. Parse TTL (1–7 days, default 1) and optional filename the same way
4. Generate a 24-char alphanumeric `fn` (`generateFilename`, rejection-sampled to stay unbiased)
5. Stream body to GCS with cache-control and optional content-disposition, keyed by `fn`
6. Insert metadata into PostgreSQL via `pgctx.Exec`
7. Return JSON: `{"ok": true, "result": {"downloadUrl": "{base_url}{fn}{sig}", "expiresAt": "..."}}`

**Auth (`auth.go`):**
- No `Authorization` header → alpha mode, project ID hardcoded as `"alpha"` (TODO: remove)
- With token → POST to `https://api.deploys.app/me.authorized` for `dropbox.upload` permission, checking `authorized` + `billingAccount.active`
- Results cached in-process for 30 seconds via `cachestore`; the external call is wrapped in `sf.Do` so concurrent uploads from the same caller collapse to one round-trip at the cache-miss edge.

**DDoS protection ladder:** see `fileHandler` / `cdnFileHandler` in `files.go`. In order from cheapest to most expensive:
1. `parseToken` HMAC check — pure CPU, no I/O.
2. `lookupFile` cache — 60s in-process cache of `(project_id, expires_at, bucket_missing)` per fn.
3. `sf.Do` around the DB `SELECT` — collapses any thundering herd at the cache-miss edge.
4. `Bucket.Attributes` — only reached for tokens that survive 1–3.

**Key libraries (same pattern as `moonrhythm/registry`):**
- `parapet` — HTTP server with middleware chain (healthz, logger, pgctx)
- `pgctx` — context-aware PostgreSQL access (`pgctx.Exec`, middleware injects DB into context)
- `cachestore` — in-process TTL cache for auth results and per-fn metadata
- `sf` — generic context-aware singleflight (`github.com/moonrhythm/sf`); used in `lookupFile` and `checkAuth` to dedupe concurrent backend calls
- `configfile` — env-var config reader (`config.MustString`, `config.StringDefault`)

## Notes

- `schema.sql` targets PostgreSQL; `project_id` is `text` (the API returns string IDs)
- `base_url` is the public download prefix (`https://dropbox.deploys.app/files/`); it shares the service host and resolves to the `GET /files/{token}` route, which streams directly or 307s to the CDN (see `cdn_base_url`)
