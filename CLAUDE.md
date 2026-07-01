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
- `cdn_base_url` — full URL prefix (including scheme and trailing slash, e.g. `https://cdn.example.com/`). When set, `GET /files/{token}` records the download metric (counted against `attrs.Size`) and 307-redirects to `{cdn_base_url}{token}`. The CDN edge is expected to fetch its origin at `https://dropbox.deploys.app/_cdn/{token}`, which streams the file (no auth, no metrics) after re-verifying the same HMAC. Each `/_cdn` response sets `Cache-Control` for the edge: success uses `public, max-age={remaining TTL}, immutable` so the edge cache lines up with the file's actual lifetime; 410/404 for known-dead URLs use `public, max-age=3600` so repeat probes are absorbed at the edge; invalid tokens get no `Cache-Control` because each garbage URL is unique and caching just burns edge slots. In-cluster callers (private/loopback/link-local `X-Real-Ip`) bypass the redirect and stream directly. Unset = original streaming behavior.
- `max_upload_size` — cap in bytes on the `maxSize` a signed-upload URL (`POST /uploads`) will accept. Unset/`0` falls back to `defaultMaxUploadSize` (5 GiB).
- `PORT` — listen port (default: `8080`)
- `log_level` — slog level (default: info)

**Download token scheme (`handler.go`):**
- The URL path component is a `token` = `fn` + `"-"` + `sig`, currently 45 chars total. `fn` is 24 random chars `[0-9A-Za-z]` (~143 bits of entropy); `sig` is 20 hex chars of HMAC-SHA256 truncated to 80 bits, keyed by `sign_key`.
- The `-` separator lets us change `fnLen` later without invalidating tokens that are already in circulation — `parseToken` splits structurally on the separator, not by fixed position. Since `fn` is alphanumeric and `sig` is hex, neither side can contain a `-`.
- `parseToken(SignKey, token)` runs first in both `fileHandler` and `cdnFileHandler` and 404s on any mismatch — DDoS attempts that don't know `sign_key` never reach the DB or GCS.
- `fn` is what we store in the bucket and the `files.fn` column. The full token is also persisted in `files.token` so the api's `dropbox.List` can rebuild download URLs without holding `sign_key`; it's otherwise only meaningful in URLs.

**Request flow (`handler.go`):**
1. Parse `Authorization` header + `project`/`projectId` from query params or `param-*` headers (query params take precedence). `project` accepts **either** a project **sid** (stable slug, e.g. `my-project`) **or** a numeric project ID; `projectId` is always the numeric ID. Because a sid always starts with a letter (api `ReValidSID`: `^[a-z][a-z0-9\-]*[^\-]$`), an all-digit `project` is unambiguously an ID, so `uploadHandler` routes it to `projectID` (an explicit `projectId` still wins). Both are relayed to `me.authorized`, which resolves `project` by sid and `projectId` by numeric ID.
2. Authorize via `checkAuth()` in `auth.go`
3. Parse TTL (1–365 days, default 1) and optional filename the same way
4. Generate a 24-char alphanumeric `fn` (`generateFilename`, rejection-sampled to stay unbiased)
5. Stream body to GCS with cache-control and optional content-disposition, keyed by `fn`
6. Insert metadata into PostgreSQL via `pgctx.Exec`
7. Return JSON: `{"ok": true, "result": {"downloadUrl": "{base_url}{fn}-{sig}", "expiresAt": "..."}}`

**Signed upload URLs (`upload_url.go`):** an alternative to `POST /` for handing a credential-free, time-limited PUT URL to a third party who uploads **straight to this service** (bytes transit here, same as `POST /`; no GCS signed URL, so no IAM setup).

1. `POST /uploads` — auth (`dropbox.upload`), generate `fn`, and mint a self-describing **upload token** = `base64url(json(uploadGrant))` + `"."` + 128-bit HMAC (keyed by `SignKey`, domain-separated from download tokens via the `uploadTokenDomain` prefix). The grant carries `fn`, `projectID`, `min/maxSize`, `contentType`, `ttl`, `filename`, `expiry`. **Writes nothing** to the DB or bucket — an unused URL leaves no trace. Returns `uploadUrl` (`{serviceRoot}/uploads/{uploadToken}`, symmetric with the `GET /files/{token}` download path), the deterministic `downloadUrl`, `ttl`, `min/maxSize`, optional `contentType`, `uploadExpiresAt`.
2. `PUT /uploads/{token}` (`uploadDirectHandler`) — **no auth header** (the signed token is the capability). `parseUploadToken` verifies the HMAC in CPU; checks expiry and (if pinned) `Content-Type`. Streams the body to GCS through `io.LimitReader(body, maxSize+1)` so a lying/absent `Content-Length` can't exceed the cap; rejects `> maxSize` (`file too large`) or `< minSize` (`file too small` / `body empty`) and **deletes the object** on any rejection (`deleteObject` runs on a `context.WithoutCancel` context — a copy error is usually a client disconnect, which already canceled `r.Context()`, and reusing it would fail the cleanup and orphan the object). On success records the row with the **real size** via `INSERT … ON CONFLICT (fn) DO UPDATE` so a replayed PUT refreshes the single row instead of adding a duplicate that would double-count storage; a failed insert logs + still returns success (the bytes are safe; served via the no-row fallback), matching `POST /`. Invalidates the per-`fn` cache (clears a pre-upload `BucketMissing` negative; per-process, others self-heal within `fileMetaCacheTTL`), bumps `upload_count`/`upload_bytes`. Storage billing (`calculateDropboxStorageUsages` sums `files.size`) is exact immediately; the `ttl` clock starts at this PUT.

The whole feature is HMAC + memblob-streamable, so it is fully testable with the in-memory bucket — no signer seam, no IAM grant. **Schema:** adds `create unique index files_fn_key on files (fn)` (`schema/04_fn_unique.sql` + `schema.sql`) — `fn` was previously unindexed, so `lookupFile` and the upsert filtered on `fn` with a full table scan (which, under SERIALIZABLE, also serialized concurrent uploads); apply it out-of-band in prod. Trade-off vs a GCS pre-signed URL: upload bandwidth flows through these pods. Accepted residuals (best-effort/bounded, not bugs): `upload_count`/`upload_bytes` may over-count a replayed PUT (observability only — `files.size` stays correct); two *concurrent* PUTs of the same token with *different* bodies can leave the stored object and the recorded size momentarily divergent; the upload token, like the download token, rides in the URL path and so appears in access logs for its ≤1h life.

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
- `streamFile` (`files.go`) serves via `http.ServeContent` over `blobReadSeeker`, a lazy `io.ReadSeeker` backed by `Bucket.NewRangeReader`. This gives HTTP `Range` support (`206` + `Content-Range` + `Accept-Ranges`, `416` on an unsatisfiable range) and conditional GETs (`Last-Modified` / `If-Range`) while reading only the requested span from GCS — a range request never reads the whole object, and `HEAD` reads nothing. Egress is billed for the bytes actually written (a `countingResponseWriter`), not `attrs.Size`.
