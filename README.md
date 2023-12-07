# Deploys.app - Dropbox

Temporary file storage

## Notes for alpha version

Currently, the parameters are in the headers.
This is not ideal, but it's a workaround for the lack of Cloudflare Workers route support.

## Development

```shell
$ asdf install
$ bun install
$ bun start
```

## Testing

```shell
$ bun run lint
$ bun run test
```

## Deployment

```shell
$ bun run deploy
```

---

## API Documentation

Endpoint: https://dropbox.deploys.app/

### Upload file

`POST /`

#### Permissions Required

- `dropbox.upload`

#### Headers

| Name           | Type     | Data Type | Description                     |
|----------------|----------|-----------|---------------------------------|
| Authorization  | required | string    | Authorization token             |
| Param-Ttl      | optional | number    | 1-7, default 1                  |
| Param-Project  | required | string    | Project name                    |
| Param-Filename | optional | string    | Filename in Content-Disposition |

#### Body

File data binary

#### Responses

##### OK

```json
{
	"ok": true,
	"result": {
		"downloadUrl": "https://dropbox.deploys.app/filename",
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
$ http -a user:pass https://dropbox.deploys.app/ \
	param-ttl:1 \
	param-project:my-project \
	< file
```

### Upload without Authorization

In alpha version, you can upload without Authorization, but its rate-limited.

#### Example HTTPie without Authorization

```shell
http https://dropbox.deploys.app/ < file
```
