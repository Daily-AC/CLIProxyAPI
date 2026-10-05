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

```yaml
keys:
  - name: alice                # usage is aggregated under this name ([A-Za-z0-9._-], max 64)
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

Both require the management key, checked by the upstream management handler with the
current `remote-management` settings (`secret-key`, `allow-remote`, `MANAGEMENT_PASSWORD`):
send `Authorization: Bearer <key>` or `X-Management-Key: <key>`. The runtime-only
`--password` local password is not accepted.

- `GET /z10/usage` returns the usage file content.
- `GET /z10/friends` returns name, models, channels, expires, enabled, active and
  last_used. Keys are never returned.

## Known limits

- Model router plugins can reroute a request after this check; none are installed on z10.
- Fields other than `model` are forwarded unchanged to the allowed channel (for example an
  OpenRouter-style `models` fallback list is up to that channel to interpret).
