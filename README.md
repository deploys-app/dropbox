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
| Param-Project   | required | string    | Project name                    |
| Param-Filename  | optional | string    | Filename in Content-Disposition |

#### Query Parameters

| Name       | Type     | Data Type | Description                     |
|------------|----------|-----------|---------------------------------|
| ttl        | optional | number    | 1-7, default 1                  |
| project    | required | string    | Project name                    |
| projectId  | optional | string    | Project ID (alternative to project name) |
| filename   | optional | string    | Filename in Content-Disposition |

> Query parameters take precedence over headers when both are provided.

#### Body

File data binary

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
