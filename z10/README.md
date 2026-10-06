# z10 friend keys

Restricted API keys for friends of the z10 deployment. Friend keys can call only
allowlisted models, served only by allowlisted channels, on allowlisted endpoints, and
their usage is counted per friend. Owner keys (`api-keys` / `access.api-keys` in
`config.yaml`) behave exactly as upstream.

The whole feature lives in this package. The only upstream change is the hook in
`cmd/server/main.go` (one import plus one line after `configaccess.Register`):

```go
serverOptions = append(serverOptions, z10.ServerOptions(configFilePath)...)
```

## friends.yaml

Place `friends.yaml` next to `config.yaml` (mode 600). Without the file the feature is off.
The admin API below creates and edits it; once it is used, the file is machine-managed:
each write regenerates the whole file from the keys in effect, so comments, formatting
and hand edits that were rejected by validation are lost.

```yaml
keys:
  - name: alice                # usage is aggregated under this name (letters, digits, _ . -; max 64; not only dots)
    key: sk-z10-...            # at least 16 characters, no whitespace
    models: ["deepseek-v4-*", "cline-pass/*"]   # '*' matches any text, case-insensitive
    channels: ["DeepSeek", "ClinePass"]         # required: openai-compatibility names
    expires: 2026-12-31        # optional; a date is valid through 23:59:59 UTC+8, or RFC3339
    enabled: true              # optional, default true
```

- `channels` is required. Each entry is an `openai-compatibility[].name` from
  `config.yaml`, matched case-insensitively. It is mapped with upstream's
  `util.OpenAICompatibleProviderKey` (for example `ClinePass` becomes
  `openai-compatible-clinepass`), the provider key under which upstream registers that
  channel's models and schedules its credentials. A name that matches no
  openai-compatibility entry is logged as a warning and grants nothing.
- A model is allowed only if its name matches a pattern **and** every provider the model
  registry lists for it is one of the key's channels. If any other channel also serves
  the name (another compat entry, an OAuth subscription such as Devin, or a prefixed
  credential that also registers bare names because `force-model-prefix` is off), the
  router could pick it, so the request is refused. A name the registry does not know is
  refused too. Model lists show only models that pass the same check.
- Validation: unique names, unique keys, no unknown fields, non-empty `models` patterns
  and `channels`. An invalid file is rejected with a logged error and the last good keys
  stay active.
- A friend entry whose key equals an owner key is dropped on every reload (logged by
  friend name, never the key) and the other entries stay active, so an owner key is never
  restricted.
- Hot reload: the config directory is watched (rename-replace saves work); changes to
  `friends.yaml` or `config.yaml` apply within about a second. Deleting the file disables
  all friend keys.
- Disabled or expired keys get `401` (`{"error":"API key disabled"}` / `"API key expired"`).
  Expiry is checked on every request, so no reload is needed when a key expires.

## What a friend key can reach

Everything else returns `403 endpoint_not_allowed`, including endpoints added upstream later.

| Endpoint | Check |
|---|---|
| `POST /v1/chat/completions`, `POST /v1/completions`, `POST /v1/responses` | body `model` |
| `POST /v1/messages`, `POST /v1/messages/count_tokens` | body `model` (cloaked Claude IDs decoded) |
| `POST /v1beta/models/{model}:generateContent` / `:streamGenerateContent` / `:countTokens` | path model (and body `model` if present) |
| `GET /v1/models`, `GET /v1beta/models` | response filtered to allowed models |
| `GET /v1/models/{model}`, `GET /v1beta/models/{model}` | `404` unless allowed |

- Any WebSocket or protocol upgrade is rejected with `403 websocket_not_allowed`
  (the Responses WebSocket chooses the model per message, which cannot be checked).
- A disallowed model returns `403 model_not_allowed`.
- The model is resolved like the router does: one thinking suffix `name(value)` is parsed
  off, the base is looked up in the model registry with its lowercase fallback, and the
  full name is tried if the base is unknown. `auto` is never allowed.
- Request bodies: at most 32 MiB before and after decoding (`413`). `Content-Encoding`
  must be absent or `identity`, or `zstd` on the OpenAI endpoints whose handlers decode
  it; anything else, including gzip, lists and repeated headers, is `415`. The parsed body
  must be exactly one JSON object (no BOM, no leading bytes, no trailing values) with at
  most one top-level key equal to `model` case-insensitively after unescaping, and that
  key must be spelled `model` with a string value (`400`). The original bytes are passed
  on unchanged.
- Credentials: upstream authentication reads the first value of `Authorization`
  (`Bearer` or raw), `X-Goog-Api-Key`, `X-Api-Key`, and the `key` and `auth_token` query
  parameters. A request is restricted when any of these five values is a friend key, even
  if another of them is an owner key. Values upstream does not read (a second
  `Authorization` header, a second `?key=`) are ignored here as well; such a request is
  authenticated by its first values only.
- The access provider accepts a friend key only when the middleware has checked the same
  request as the same friend, so a key that becomes a friend key during a reload is
  rejected with `401` instead of passing unrestricted.

## Usage

Requests (and failures, final HTTP status >= 400) are counted per inbound request that
passed the checks; tokens come from upstream usage records whose principal is
`friend:<name>`. Rejected requests and owner usage are never recorded. Data is kept per
friend, per model (at most 200 models, further ones under `_other`) and per UTC+8 day
(latest 400 days), and written atomically to `z10-usage.json` next to `config.yaml` at
most 5 seconds after a change, and on every change once SIGINT/SIGTERM is received.

## Admin routes

All routes require the management key, checked by the upstream management handler with
the current `remote-management` settings (`secret-key`, `allow-remote`,
`MANAGEMENT_PASSWORD`): send `Authorization: Bearer <key>` or `X-Management-Key: <key>`.
The runtime-only `--password` local password is not accepted. Friend keys get `403`.
Every response has `Cache-Control: no-store`; errors are `{"error":"<message>"}`.

- `GET /z10/usage` returns the usage file content.
- `GET /z10/channels` returns `{"channels":[{"name","models"}]}`: the openai-compatibility
  entries of `config.yaml` that are not disabled, in config order, with the model IDs the
  model registry currently lists as available for each (sorted). Only these channels can be
  granted to friends; OAuth, subscription and other API key providers never appear.
- `GET /z10/friends` returns `{"friends":[...]}` in file order. Each item has `name`,
  `models`, `channels`, `expires` (omitted when never), `enabled`, `active`,
  `inactive_reason` (`"disabled"`, `"expired"` or `""`), `last_used` (omitted when never),
  and the usage totals `requests`, `failed_requests`, `total_tokens`.
- `POST /z10/friends` with `{"name", "channels", "models"?, "expires"?, "enabled"?}`
  creates a friend and returns `201 {"friend": <item>, "key": "sk-z10-..."}`. The server
  generates the key (`sk-z10-` + 32 random bytes, unpadded base64url); this response is the
  only place a key is ever returned, and the request log records it as `<redacted>`.
  `models` defaults to `["*"]`, `expires` to never (`""`), `enabled` to `true`.
- `PATCH /z10/friends/{name}` with any of `{"enabled", "expires", "channels", "models"}`
  returns `200 {"friend": <item>}`. Absent or `null` fields are unchanged; `"expires": ""`
  removes the expiry. Names and keys cannot be changed.
- `DELETE /z10/friends/{name}` returns `204`. The friend's usage history is kept.

Validation (`400` unless noted): the request body is one JSON object without unknown
fields; `name` matches `^[\p{L}\p{N}_.-]{1,32}$` and is not only dots, and is unique
(`409`); `channels` is non-empty and each one is an enabled openai-compatibility entry
(matched case-insensitively and stored as spelled in `config.yaml`); `models` is non-empty
without blank patterns; `expires` is `YYYY-MM-DD` or RFC3339. An unknown name is `404`.
`POST` and `PATCH` require `Content-Type: application/json` (`415` otherwise, which blocks
HTML form posts); a `DELETE` may omit it but must not send another type.

Writes are serialized with reloads. Each write starts from the keys in effect, validates
the complete new file with the same parser as the loader (including the owner key rule),
writes it to a temp file (mode 600, fsync) and renames it over `friends.yaml`, creating
the file if needed. The new keys apply immediately; the watcher's reload of the same file
changes nothing. When validation or the write fails, nothing changes. Logs name the
friend (`z10: friend created|updated|deleted name=<name>`), never the key.

## Known limits

- Model router plugins can reroute a request after this check; none are installed on z10.
- Fields other than `model` are forwarded unchanged to the allowed channel (for example an
  OpenRouter-style `models` fallback list is up to that channel to interpret).
