# Deploys.app - Dropbox

Temporary file storage

## Development

```shell
$ asdf install
$ go run .
```

## Testing

```shell
$ go test ./...
```

## Deployment

Docker image is built and pushed automatically on push to `main`. See `.github/workflows/build.yaml`.

**Required environment variables:**

| Variable      | Description                                              |
|---------------|----------------------------------------------------------|
| `db_url`      | PostgreSQL connection string                             |
| `bucket_name` | GCS bucket name                                          |
| `sign_key`    | HMAC key for signing download tokens. Rotating invalidates every outstanding URL. |
| `base_url`    | Download URL prefix (default: `https://dropbox.deploys.app/files/`) |
| `internal_secret` | Bearer token guarding `POST /internal/gc`. When unset, the endpoint is unauthenticated. |
| `max_upload_size` | Cap (bytes) on the `maxSize` a signed-upload URL will accept. Unset/`0` → 5 GiB. |
| `PORT`        | Listen port (default: `8080`)                            |

---

## API Documentation

Endpoint: https://dropbox.deploys.app/

### Upload file

`POST /`

#### Permissions Required

- `dropbox.upload`

#### Headers

| Name            | Type     | Data Type | Description                     |
|-----------------|----------|-----------|---------------------------------|
| Authorization   | required | string    | Authorization token             |
| Param-Ttl       | optional | number    | 1-7, default 1                  |
| Param-Project   | required | string    | Project sid (stable slug) or numeric project ID |
| Param-Filename  | optional | string    | Filename in Content-Disposition |

#### Query Parameters

| Name       | Type     | Data Type | Description                     |
|------------|----------|-----------|---------------------------------|
| ttl        | optional | number    | 1-7, default 1                  |
| project    | required | string    | Project sid (stable slug) **or** numeric project ID — an all-digit value is treated as the ID |
| projectId  | optional | string    | Numeric project ID (same as passing a numeric `project`) |
| filename   | optional | string    | Filename in Content-Disposition |

> Query parameters take precedence over headers when both are provided.

#### Body

File data binary. The body must be non-empty — an upload that carries zero
bytes (including a chunked / unknown-length request that turns out to be empty)
is rejected with `body empty` and stores nothing.

#### Responses

##### OK

```json
{
	"ok": true,
	"result": {
		"downloadUrl": "https://dropbox.deploys.app/files/<token>",
		"expiresAt": "2020-01-01T01:01:01Z"
	}
}
```

`<token>` is `{fn}-{sig}` (currently 45 chars): a 24-char random alphanumeric filename, a `-` separator, and a 20-char HMAC-SHA256 signature (keyed by `sign_key`). Tampered or made-up tokens are rejected before any DB or storage lookup. The separator means future changes to filename length stay backward-compatible.

##### Unauthorized

```json
{
	"ok": false,
	"error": {
		"message": "api: unauthorized"
	}
}
```

#### Example HTTPie

```shell
# using query parameters
$ http POST https://dropbox.deploys.app/?ttl=1&project=my-project \
	Authorization:"Bearer <token>" \
	< file

# using headers
$ http POST https://dropbox.deploys.app/ \
	Authorization:"Bearer <token>" \
	param-ttl:1 \
	param-project:my-project \
	< file
```

---

### Signed upload URL

To let a third party (a browser, a model-built tool, a CI job) upload a file
without holding a deploys.app credential — and without routing the bytes through
the caller's own backend — mint a short-lived **signed upload URL** on this
service and hand it over. The holder `PUT`s the file straight here; the service
enforces the signed size/content-type limits while streaming the body to storage
and records the file in one request.

The URL is signed with this service's own HMAC key (no GCS signed URL, so no IAM
setup), and the upload bytes flow through this service (the same as `POST /`).

Two steps: **create** the URL, then **PUT** the file to it.

#### 1. Create — `POST /uploads`

##### Permissions Required

- `dropbox.upload`

##### Headers

| Name          | Type     | Description         |
|---------------|----------|---------------------|
| Authorization | required | Authorization token |

##### Body (JSON)

| Field         | Type   | Description                                                                 |
|---------------|--------|-----------------------------------------------------------------------------|
| `project`     | string | Project sid **or** numeric project ID (all-digit ⇒ ID). Required.           |
| `projectId`   | string | Numeric project ID (same as a numeric `project`).                           |
| `ttl`         | number | Download lifetime of the resulting file, 1–7 days (default 1).              |
| `filename`    | string | Optional `Content-Disposition` filename on download.                        |
| `contentType` | string | Optional. When set, the PUT **must** send this exact `Content-Type`.        |
| `minSize`     | number | Optional lower bound in bytes (clamped to ≥ 1, so empty uploads are refused).|
| `maxSize`     | number | Optional upper bound in bytes (clamped to `max_upload_size`, default 5 GiB).|
| `expires`     | number | Optional upload-URL validity in seconds (1 s – 1 h, default 900).           |

##### OK

```json
{
	"ok": true,
	"result": {
		"method": "PUT",
		"uploadUrl": "https://dropbox.deploys.app/uploads/<upload-token>",
		"downloadUrl": "https://dropbox.deploys.app/files/<token>",
		"contentType": "application/pdf",
		"minSize": 1,
		"maxSize": 1048576,
		"ttl": 1,
		"uploadExpiresAt": "2020-01-01T01:16:01Z"
	}
}
```

`uploadExpiresAt` is when the upload URL stops working (now + `expires`). The
`downloadUrl` is known up front but only serves content after a successful PUT.
`contentType` is present only when one was pinned; if so, the PUT must send it
verbatim. Nothing is written to the DB or storage at this step — an upload URL
that is never used leaves nothing behind.

#### 2. Upload — `PUT {uploadUrl}`

`PUT` the file as the request body. The service enforces the signed limits and,
on success, returns the live download URL and the file's size and expiry:

- a body below `minSize` → `file too small` (an empty body → `body empty`),
- a body above `maxSize` → `file too large` (rejected even with a missing or
  lying `Content-Length` — the stream is hard-capped),
- a `Content-Type` that doesn't match a pinned `contentType` → `content type mismatch`,
- a token that is malformed/forged → `invalid upload token`; one past
  `uploadExpiresAt` → `upload url expired`.

No `Authorization` header is needed — a valid signed token is the capability.
Re-`PUT`ting the same URL before it expires overwrites the file (the size is
re-recorded; no duplicate row).

```shell
$ curl -X PUT "<uploadUrl>" \
	-H "Content-Type: application/pdf" \
	--data-binary @file.pdf
```

```json
{
	"ok": true,
	"result": {
		"downloadUrl": "https://dropbox.deploys.app/files/<token>",
		"size": 12345,
		"expiresAt": "2020-01-02T01:01:01Z"
	}
}
```

The file's `ttl`-day download clock starts on this successful PUT, and storage
billing sees the real size immediately. On a multi-pod deployment, if the
`downloadUrl` was probed before the upload landed, that pod may briefly (≤ 60 s)
still `404` the file until its per-process cache expires.

---

## Garbage collection

`POST /internal/gc`

Deletes every file whose `expires_at` is in the past from both GCS and PostgreSQL, then returns `204 No Content`. Storage objects that are already gone are ignored, so re-running is safe. This is the only mechanism that reclaims expired files — nothing runs it on a timer inside the service, so schedule an external caller.

When `internal_secret` is set, the request must carry `Authorization: Bearer <internal_secret>`; otherwise it returns `401`. Leave `internal_secret` unset only if the route is unreachable from outside the cluster.

### Schedule with Cloud Scheduler

Run it hourly with a [Cloud Scheduler](https://cloud.google.com/scheduler) HTTP job:

```shell
$ gcloud scheduler jobs create http dropbox-gc \
	--location=asia-southeast1 \
	--schedule="0 * * * *" \
	--time-zone="Etc/UTC" \
	--uri="https://dropbox.deploys.app/internal/gc" \
	--http-method=POST \
	--headers="Authorization=Bearer <internal_secret>"
```

- `--location` must be a region Cloud Scheduler supports; it does not have to match where the service runs.
- `--schedule` is a standard cron expression — `0 * * * *` fires at the top of every hour.
- `--uri` must resolve to a host the job can reach. If `/internal/gc` is only exposed inside the cluster, target the in-cluster address instead and the job will need network access to it.
- Set `--headers` to match `internal_secret`. Update the job with `gcloud scheduler jobs update http dropbox-gc --headers=...` whenever the secret rotates.

Trigger a one-off run to verify the job:

```shell
$ gcloud scheduler jobs run dropbox-gc --location=asia-southeast1
```
