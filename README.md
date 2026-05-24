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

`<token>` is a 44-char string: a 24-char random alphanumeric filename concatenated with a 20-char HMAC-SHA256 signature (keyed by `sign_key`). Tampered or made-up tokens are rejected before any DB or storage lookup.

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
