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

**Environment variables:**

| Variable       | Required | Description                                                                              |
|----------------|----------|------------------------------------------------------------------------------------------|
| `db_url`       | yes      | PostgreSQL connection string                                                             |
| `bucket_name`  | yes      | GCS bucket name                                                                          |
| `base_url`     | no       | Download URL prefix (default: `https://dropbox.deploys.app/files/`)                     |
| `api_endpoint` | no       | deploys.app API base URL (default: `https://api.deploys.app`)                           |
| `PORT`         | no       | Listen port (default: `8080`)                                                            |
| `log_level`    | no       | Log level: `debug`, `info`, `warn`, `error` (default: `info`)                           |

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
		"downloadUrl": "https://dropbox-files.deploys.app/1<filename>",
		"expiresAt": "2020-01-01T01:01:01Z"
	}
}
```

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
