# API reference

The gateway exposes two planes on one listener:

- **Data-plane** — OpenAI- and Anthropic-compatible inference endpoints,
  authenticated by API key.
- **Control-plane** — the console's JSON API and the SPA itself, authenticated
  by an HMAC-signed session cookie.

## Authentication

| Surface | Mechanism | How |
|---------|-----------|-----|
| Data-plane (`/v1/*`) | API key | `Authorization: Bearer <key>` or `x-api-key: <key>` |
| Control-plane (`/api/*`, SPA) | Session cookie | obtained from `POST /auth/login` (local mode) or OIDC (`GET /auth/sso` → `GET /auth/callback`) |

Access tiers on the control-plane:

- **session** — any authenticated user (self-service).
- **admin** — `airllm_admin`.
- **auditor** — `airllm_auditor` (admin also passes the auditor gate).

API keys are formatted `air_<env>_<random>`, stored as a SHA-256 hash plus a
prefix and last-4, and shown in full exactly once at creation.

## Public

| Method | Path | Purpose |
|--------|------|---------|
| `GET` | `/healthz` | Liveness |
| `GET` | `/readyz` | Readiness (datastore reachability) |
| `GET` | `/` | The SPA console (static, embedded) |
| `GET` | `/api/auth/mode` | Reports the active auth mode (`local` or `oidc`) and, in OIDC mode, the SSO start URL. The SPA uses this to decide whether to render a password form or an "Sign in with SSO" button. |
| `POST` | `/auth/login` | Password login (local mode only) → sets an HMAC session cookie. Body: `{"username","password"}` |
| `GET` | `/auth/sso` | Begin OIDC login — redirects to the IdP (OIDC mode only). PKCE + `state` + `nonce` are set in short-lived signed cookies. |
| `GET` | `/auth/callback` | OIDC callback — validates `state`, exchanges the code, verifies the ID token, upserts the user, sets the session cookie, and redirects to `/` (OIDC mode only). |
| `POST` | `/auth/logout` | Clears the session cookie |

## Data-plane (API key)

| Method | Path | Purpose |
|--------|------|---------|
| `POST` | `/v1/chat/completions` | OpenAI Chat Completions. Non-streaming and SSE (`stream: true`). Accepts standard OpenAI multi-part vision content (`content: [{"type":"text",...},{"type":"image_url",...}]`). |
| `GET` | `/v1/models` | OpenAI model list, filtered by the key's allowed models |
| `POST` | `/v1/messages` | Anthropic Messages. Non-streaming and SSE. Accepts Anthropic's own image content blocks (`{"type":"image","source":{"type":"base64",...}}` or `{"type":"image","source":{"type":"url",...}}`). |
| `POST` | `/v1/audio/transcriptions` | OpenAI-shaped batch speech-to-text (`multipart/form-data`: `model`, `file`, optional `language`/`prompt`). Always responds `{"text": "..."}`; `response_format` is accepted but ignored. |
| `POST` | `/v1/audio/speech` | OpenAI-shaped batch text-to-speech (JSON: `model`, `input`, optional `voice`/`response_format`). Responds with raw audio bytes and the upstream `Content-Type`. |

The `model` field accepts a configured **alias** (e.g. `mock-gpt`) or, when the
key's role allows passthrough, an explicit `provider/model`. Cross-protocol
calls are translated; same-protocol calls pass through. See
[`translation.md`](translation.md).

**Vision / image content**: both ingress protocols accept images in their
respective native multi-part content formats and forward them to the real
upstream unchanged. Two things to know: (1) this gateway does not verify
that an alias's target model is actually vision-capable before making the
call — a text-only model gets a live error from the real upstream, not a
clean local one; (2) the practical image size ceiling is the existing
16 MiB request body cap (`internal/httpapi/server.go`'s `maxRequestBody`) —
there's no separate, larger limit for vision requests.

Errors use the caller's protocol shape (OpenAI error object vs Anthropic error
object). When every routing target is busy the gateway returns `429` rather
than failing.

### Reasoning tokens

`completion_tokens` (`output_tokens` on the Anthropic ingress) is **every token
billed at the output rate**, including whatever the model spent thinking. That
holds regardless of how the upstream reported it: OpenAI-shaped vendors count
thinking inside `completion_tokens`, while Vertex AI leaves it out and lets it
show only in `total_tokens`, and the gateway reconciles both before metering.

When a response did any thinking, the OpenAI ingress also reports the share:

```json
"usage": {
  "prompt_tokens": 10,
  "completion_tokens": 176,
  "total_tokens": 186,
  "completion_tokens_details": {"reasoning_tokens": 146}
}
```

`completion_tokens_details` is **omitted entirely** when there was no thinking,
so a response from a non-reasoning model carries the same three fields it
always did. One related tidy-up: `total_tokens` is now always
`prompt_tokens + completion_tokens`, where an upstream that omitted its own
total used to leave the client a `total_tokens` of `0` beside non-zero parts. The reasoning count is a breakdown of `completion_tokens`, not an addition
to it — adding the two double-counts. Operators see the same split as
`tokens_reasoning` in the usage breakdown; see
[Operations → Reasoning tokens](operations.md#reasoning-tokens).

## Control-plane — self-service (session)

| Method | Path | Purpose |
|--------|------|---------|
| `GET` | `/api/me` | Current principal (subject, email, roles, is_admin) |
| `POST` | `/api/me/password` | Change own password. Body: `{"current","new"}`. Local-auth users only; OIDC-provisioned users (`auth_source=oidc`) are rejected. Requires the caller's current password (not admin-only). |
| `GET` | `/api/keys` | List the caller's API keys |
| `POST` | `/api/keys` | Create a key (token returned once). Body: `{"name"}` |
| `POST` | `/api/keys/{id}/revoke` | Revoke one of the caller's keys |
| `GET` | `/api/usage` | The caller's rolling-window usage (tokens + cost) |
| `GET` | `/api/usage/breakdown` | The caller's usage grouped by provider and by model over the last `hours` (default 24, max 168). Returns `{"providers":[...],"models":[...]}`; each row carries `tokens_in`, `tokens_out` and `tokens_reasoning` — see [Reasoning tokens](#reasoning-tokens) |

## Control-plane — admin (`airllm_admin`)

| Method | Path | Purpose |
|--------|------|---------|
| `GET` | `/api/admin/users` | List users. Each entry now includes `disabled`, `auth_source` (`local` or `oidc`), and `display`. |
| `POST` | `/api/admin/users` | Create a local user. Body: `{"username","email","display","roles","password"}`. Password must be ≥ 8 characters; roles must be known keys. |
| `PUT` | `/api/admin/users/{id}` | Update `email`, `display`, `roles`, or `disabled`. Cannot set a password via this route; use the `/password` sub-resource. |
| `POST` | `/api/admin/users/{id}/password` | Admin-reset a user's password (no current-password required). Body: `{"password":"..."}`. Blocked for OIDC-provisioned users. |
| `DELETE` | `/api/admin/users/{id}` | Delete a user. Blocked if the user still owns active API keys (revoke them first). Blocked if deleting would remove the last admin. Prefer setting `disabled=true` as a non-destructive alternative. |
| `GET` | `/api/admin/keys` | List all keys |
| `POST` | `/api/admin/keys/{id}/revoke` | Revoke any key |
| `GET` | `/api/admin/usage` | Usage across all keys |
| `GET` | `/api/admin/usage/breakdown` | Usage across all keys grouped by provider and by model over the last `hours` (default 24, max 168). Returns `{"providers":[...],"models":[...]}`; each row carries `tokens_in`, `tokens_out` and `tokens_reasoning` — see [Reasoning tokens](#reasoning-tokens) |
| `GET`/`PUT` | `/api/admin/roles` · `/api/admin/roles/{role}` | Role policies (allowed models, passthrough, limits) |
| `GET`/`PUT`/`DELETE` | `/api/admin/aliases` · `/api/admin/aliases/{alias}` | Model alias catalog (targets, strategy, fallback tiers) |
| `GET`/`PUT` | `/api/admin/providers` · `/api/admin/providers/{name}` | Providers (kind, base URL, structured `config`, sealed credential, max concurrency, enabled). See [Provider fields](#provider-fields) below and [Provider kinds](configuration.md#provider-kinds). |
| `GET` | `/api/admin/providers/{name}/models` | Upstream model ids for one provider, for the alias editor's dropdown (5-min cache; `unsupported: true` when the kind cannot list). Live from the vendor's catalogue for the OpenAI-compatible kinds; for `vertex` a short **curated** list, since its compatibility surface publishes none — a model missing from it can still be typed by hand. |
| `GET`/`PUT` | `/api/admin/pricing` · `/api/admin/pricing/{model}` | Per-provider/model pricing (USD per 1M of the row's unit — `tokens`, `audio_second`, or `text_char`); provider `""` = any. A `tokens` row may carry a long-prompt tier: `context_threshold` prompt tokens above which `input_per_1m_above`/`output_per_1m_above` price the whole call instead — see [Long-prompt price tiers](configuration.md#long-prompt-price-tiers). A threshold with either above-rate left at zero is rejected with `400` |
| `POST` | `/api/admin/pricing/import/{provider}` | Import a provider's whole catalog pricing (e.g. OpenRouter, which publishes it) into the pricing table. `{"imported": N}`, or `{"imported": 0, "unsupported": true}` when the provider's kind doesn't publish pricing |
| `GET`/`PUT` | `/api/admin/dlp` | DLP policy (incl. Sensitive Info Detection patterns + custom patterns) |
| `GET` | `/api/admin/dlp/patterns` | Catalog of toggleable detection patterns (built-ins + model toggles) |
| `GET` | `/api/admin/dlp/incidents` | Recent DLP incidents (secret-free samples) |
| `GET`/`PUT` | `/api/admin/capture` | Capture policy |
| `GET`/`PUT` | `/api/admin/secondpass` | Second-pass (flywheel) policy |
| `GET` | `/api/admin/webhooks` · `POST` · `DELETE /{id}` | Alert webhook endpoints (HMAC-signed delivery) |
| `POST` | `/api/admin/dataset/export` | Export reviewed captures as a labeled JSONL training artifact |
| `GET` | `/api/admin/audit` | Admin audit log |

### Provider fields

`PUT /api/admin/providers/{name}` takes `kind`, `base_url`, `enabled`,
`max_concurrency`, and three fields worth spelling out:

| Field | Meaning |
|-------|---------|
| `config` | Kind-specific structured configuration, stored as JSON. Today only `vertex` uses it, for `{"project": "...", "location": "..."}`. **A save that omits it keeps what is stored** — unlike `base_url` or `enabled`, which are replaced wholesale — so a client that knows nothing about a kind's configuration cannot erase it as a side effect of an unrelated edit. A configuration that could never serve a request is rejected on save. |
| `api_key` | Static key for the OpenAI-compatible kinds. Blank keeps the stored credential. |
| `credential_json` | A Google service-account JSON key for `vertex`. Blank keeps the stored credential — and, when nothing is stored, means the gateway authenticates as its own federated identity. |

`api_key` and `credential_json` seal into the same column, so a request setting
both is rejected rather than resolved by picking one.

`GET /api/admin/providers` returns `config` verbatim (it holds a cloud project
and location, not secrets) and reports the credential only as
`has_credential` — reading the configuration never discloses it.

## Control-plane — audit (`airllm_auditor`)

| Method | Path | Purpose |
|--------|------|---------|
| `GET` | `/api/audit/captures` | List captures (metadata + DLP labels, no body) |
| `GET` | `/api/audit/captures/{id}` | One capture with its decrypted body (the view is access-logged) |
| `GET` | `/api/audit/review` | The review queue (unreviewed + second-pass discrepancies) |
| `POST` | `/api/audit/captures/{id}/review` | Set `review_status` (+ optional gold labels). `404` on unknown id. Body: `{"review_status","labels"}` |

`review_status` is one of `confirmed`, `false_positive`, `false_negative`,
`unreviewed`.

See [DLP, capture & audit](dlp-capture-audit.md) for how captures, reviews, and
the second-pass interact.
