# z10 friend keys

Restricted API keys for friends of the z10 deployment. Friend keys can call only
allowlisted models on allowlisted endpoints, and their usage is counted per friend.
Owner keys (`api-keys` / `access.api-keys` in `config.yaml`) behave exactly as upstream.

The whole feature lives in this package. The only upstream change is the hook in
`cmd/server/main.go` (one import plus one line after `configaccess.Register`):

```go
serverOptions = append(serverOptions, z10.ServerOptions(configFilePath)...)
```

## friends.yaml

Place `friends.yaml` next to `config.yaml` (mode 600). Without the file the feature is off.

```yaml
keys:
  - name: alice                # usage is aggregated under this name ([A-Za-z0-9._-], max 64)
    key: sk-z10-...            # at least 16 characters, no whitespace
    models: ["deepseek-v4-*", "cline-pass/*"]   # '*' matches any text, case-insensitive
    expires: 2026-12-31        # optional; a date is valid through 23:59:59 UTC+8, or RFC3339
    enabled: true              # optional, default true
```

- Validation: unique names, unique keys, no key equal to an owner key, no unknown fields.
- Hot reload: the config directory is watched (rename-replace saves work); changes to
  `friends.yaml` or `config.yaml` apply within about a second. An invalid file is
  rejected with a logged error and the last good keys stay active. Deleting the file
  disables all friend keys.
- Disabled or expired keys get `401` (`{"error":"API key disabled"}` / `"API key expired"`).
  Expiry is checked on every request, so no reload is needed when a key expires.

## What a friend key can reach

Everything else returns `403 endpoint_not_allowed`, including endpoints added upstream later.

| Endpoint | Check |
|---|---|
| `POST /v1/chat/completions`, `POST /v1/completions`, `POST /v1/responses` | body `model` |
| `POST /v1/messages`, `POST /v1/messages/count_tokens` | body `model` |
| `POST /v1beta/models/{model}:generateContent` / `:streamGenerateContent` / `:countTokens` | path model (and body `model` if present) |
| `GET /v1/models`, `GET /v1beta/models` | response filtered to allowed models |
| `GET /v1/models/{model}`, `GET /v1beta/models/{model}` | `404` unless allowed |

- Any WebSocket or protocol upgrade is rejected with `403 websocket_not_allowed`
  (the Responses WebSocket chooses the model per message, which cannot be checked).
- A model outside the allowlist returns `403 model_not_allowed` naming the model.
- The body is inspected raw, as decoded by upstream (`zstd`), and gunzipped; every
  `model` found in any view must be allowed, and the original bytes are passed on unchanged.
- Model names are normalized before matching: trim, drop a leading `models/`, decode
  Claude list-cloaked IDs (`claude-fable-5-dd-<reversed>`), strip one thinking suffix
  `name(value)`, lowercase.
- Credentials are read from the same places as upstream: `Authorization: Bearer`,
  `X-Goog-Api-Key`, `X-Api-Key`, `?key=`, `?auth_token=`. A request carrying any friend
  key is restricted, even if it also carries an owner key.

## Usage

Requests (and failures, final HTTP status >= 400) are counted per inbound request by the
middleware; tokens come from upstream usage records whose principal is `friend:<name>`.
Owner usage is never recorded. Data is kept per friend, per model and per UTC+8 day,
and written atomically to `z10-usage.json` next to `config.yaml` at most 5 seconds after
a change, and on every change once SIGINT/SIGTERM is received.

## Admin routes

Both require the management key, checked by the upstream management handler with the
current `remote-management` settings (`secret-key`, `allow-remote`, `MANAGEMENT_PASSWORD`):
send `Authorization: Bearer <key>` or `X-Management-Key: <key>`. The runtime-only
`--password` local password is not accepted.

- `GET /z10/usage` returns the usage file content.
- `GET /z10/friends` returns name, models, expires, enabled, active and last_used.
  Keys are never returned.
