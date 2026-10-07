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
| `GET` | `/readyz` | Readiness (datastore reachability; a Postgres outage shorter than `LOOKUP_CACHE_MAX_STALE` is tolerated) |
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
| `POST` | `/v1/audio/transcriptions` | OpenAI-shaped batch speech-to-text (`multipart/form-data`: `model`, `file`, optional `language`/`prompt`/`alternative_languages`/`response_format`). `language` is a BCP-47 tag (`en`, `en-US`); `alternative_languages` lists further ones — repeat the field, separate with commas, or spell it `alternative_languages[]` — and is passed to recognisers that take several languages (`google-speech`). An OpenAI-compatible (Whisper-family) tier, which would translate speech into a forced `language`, is then sent no language and detects it; a detection outside `language` + `alternative_languages` (or none) decodes once more with `language` forced, at the cost of a second upstream call. `response_format=verbose_json` answers `{"task":"transcribe","text","language","confidence","duration"}` from whichever tier served: `language` a BCP-47 tag — the requested one when the provider did not say, or named a requested language (primary or alternative) without its region (the Whisper family answers `english` for `en-US`), so the tag keeps its form across a failover — `confidence` 0..1 (`0` = the provider reported none; for the Whisper family it is derived from the segments' log-probabilities), `duration` in seconds. Any other format answers `{"text": "..."}`. On an alias with `expose_backend_headers`, `X-Backend-Model` is the display label of the target that served, after any failover (none for a target without a label). |
| `GET` | `/v1/audio/capabilities?model=<alias>` | What the alias's **first tier** can do: `{"model": "...", "recognition": {"languages": ["en", "ru", …]}, "synthesis": {"voices": [{"id": "en-US-Chirp3-HD-Charon", "language": "en-US", "gender": "male"}, …]}}`. Languages are BCP-47 primary subtags, answered by the first target of that tier which can transcribe. Voices are answered by the first target which can synthesize: the canonical voices its [`voices` option](configuration.md#failover-policy-getput-apiadminfailover) lists, else the provider's own catalogue (`google-tts`), else none; `gender` is `male`, `female`, `neutral` or empty when unknown. `recognition` / `synthesis` is absent when the first tier cannot transcribe / synthesize; a provider failing to list its voices answers like a failed upstream call. Same key permission check as the other routes. |
| `POST` | `/v1/audio/speech` | OpenAI-shaped batch text-to-speech (JSON: `model`, `input`, optional `voice`/`language`/`response_format`). Responds with raw audio bytes. `response_format` defaults to `wav`: every tier then answers `audio/wav` with the sample rate in the WAV header (each provider's own — Google 24 kHz, Piper often 22.05 kHz), and a tier that returns anything else counts as failed. Other formats are passed on with the upstream `Content-Type`. `voice` is the canonical voice; each tier speaks it as its [`voices` / `default_voices` options](configuration.md#failover-policy-getput-apiadminfailover) map it, and a tier that cannot — or whose upstream refuses the name — is passed over for the next. `language` (BCP-47) picks the default voice for a voice whose name carries no language. On an alias with the [synthesis cache](configuration.md#synthesis-cache), a request the serving tier has answered before is answered from its cached clip. On an alias with `expose_backend_headers`, `X-Backend-Model` is the display label of the target that spoke — after any failover, and for a cached clip the label the target that rendered it had then (none for a target without a label). |

The `model` field accepts a configured **alias** (e.g. `mock-gpt`) or, when the
key's role allows passthrough, an explicit `provider/model`. Cross-protocol
calls are translated; same-protocol calls pass through. See
[`translation.md`](translation.md).

**Vision / image content**: both ingress protocols accept images in their
respective native multi-part content formats and forward them to the real
upstream unchanged. Three things to know: (1) this gateway does not verify
that an alias's target model is actually vision-capable before making the
call — a text-only model gets a live error from the real upstream, not a
clean local one; (2) the practical image size ceiling is the existing
16 MiB request body cap (`internal/httpapi/server.go`'s `maxRequestBody`) —
there's no separate, larger limit for vision requests; (3) an image inside
an Anthropic `tool_result` block is captured (not dropped) on ingress, but
every real upstream is reached through the OpenAI-shaped client regardless
of protocol, and OpenAI's own tool-message schema only accepts text — so
that image is dropped (with a warning logged) at the egress encoding step,
never reaching the model. A top-level user-turn image is unaffected.

Errors use the caller's protocol shape (OpenAI error object vs Anthropic error
object). When every routing target is busy the gateway returns `429` rather
than failing.

A client may send an `X-Session-Id` header (up to 128 characters) to tie its
requests together, e.g. every turn of one phone call. The gateway logs it on
each failed target attempt and records it in the usage ledger's `session`
column, so one session's requests and failovers can be traced together. On an
alias with session affinity on, it also keeps a session that a fallback tier
served on that tier — see [Call affinity](configuration.md#call-affinity).

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
| `PUT` | `/api/admin/users/{id}` | Partial update of `email`, `display`, `roles`, `disabled` — a field left out of the body keeps its current stored value (an explicit `[]`/`false`/`true` still applies). Cannot set a password via this route; use the `/password` sub-resource. |
| `POST` | `/api/admin/users/{id}/password` | Admin-reset a user's password (no current-password required). Body: `{"password":"..."}`. Blocked for OIDC-provisioned users. |
| `DELETE` | `/api/admin/users/{id}` | Delete a user. Blocked if the user still owns active API keys (revoke them first). Blocked if deleting would remove the last admin. Prefer setting `disabled=true` as a non-destructive alternative. |
| `GET` | `/api/admin/keys` | List all keys |
| `POST` | `/api/admin/keys/{id}/revoke` | Revoke any key |
| `GET` | `/api/admin/usage` | Usage across all keys |
| `GET` | `/api/admin/usage/breakdown` | Usage across all keys grouped by provider and by model over the last `hours` (default 24, max 168). Returns `{"providers":[...],"models":[...]}`; each row carries `tokens_in`, `tokens_out` and `tokens_reasoning` — see [Reasoning tokens](#reasoning-tokens) |
| `GET`/`PUT` | `/api/admin/roles` · `/api/admin/roles/{role}` | Role policies (allowed models, passthrough, limits) |
| `GET`/`PUT`/`DELETE` | `/api/admin/aliases` · `/api/admin/aliases/{alias}` | Model alias catalog (targets, strategy, fallback tiers). Each target carries a free-form `options` object (`timeout_ms`, `fallback_on_auth`, `breaker`, `thinking`, …) — see [Failover policy](configuration.md#failover-policy-getput-apiadminfailover). The alias itself carries `session_affinity` (default `false`) and `session_affinity_ttl_s` (`0` = the 4-hour default, at most 604800) — see [Call affinity](configuration.md#call-affinity). It also carries `synthesis_cache` (default `false`) and `synthesis_cache_ttl_s` (`0` = the 7-day default, at most 2592000) — see [Synthesis cache](configuration.md#synthesis-cache) |
| `GET` | `/api/admin/aliases/{alias}/health` | Circuit breaker of every tier of the alias: `state` (`closed`/`open`/`half_open`), `reason`, `open_until`, `cooldown_ms`, `trips`, failure counters and the tier's targets — see [Circuit breaker](configuration.md#circuit-breaker) |
| `POST` | `/api/admin/aliases/{alias}/tiers/{tier}/release` | Close a quarantined tier by hand (`tier` = its configured priority) and forget its trip history; audited as `alias.tier.release` |
| `GET`/`PUT` | `/api/admin/providers` · `/api/admin/providers/{name}` | Providers (kind, base URL, structured `config`, sealed credential, max concurrency, enabled). See [Provider fields](#provider-fields) below and [Provider kinds](configuration.md#provider-kinds). |
| `GET` | `/api/admin/providers/{name}/models` | Upstream model ids for one provider, for the alias editor's dropdown (5-min cache; `unsupported: true` when the kind cannot list). Live from the vendor's catalogue for the OpenAI-compatible kinds; for `vertex`, `google-speech` and `google-tts` a short **curated** list, since none publishes one — a model missing from it can still be typed by hand. |
| `GET`/`PUT` | `/api/admin/pricing` · `/api/admin/pricing/{model}` | Per-provider/model pricing (USD per 1M of the row's unit — `tokens`, `audio_second`, or `text_char`); provider `""` = any. A `tokens` row may carry a long-prompt tier: `context_threshold` prompt tokens above which `input_per_1m_above`/`output_per_1m_above` price the whole call instead — see [Long-prompt price tiers](configuration.md#long-prompt-price-tiers). A threshold with either above-rate left at zero is rejected with `400` |
| `POST` | `/api/admin/pricing/import/{provider}` | Import a provider's whole catalog pricing (e.g. OpenRouter, which publishes it) into the pricing table. `{"imported": N}`, or `{"imported": 0, "unsupported": true}` when the provider's kind doesn't publish pricing |
| `GET`/`PUT` | `/api/admin/dlp` | DLP policy (incl. Sensitive Info Detection patterns + custom patterns) |
| `GET` | `/api/admin/dlp/patterns` | Catalog of toggleable detection patterns (built-ins + model toggles) |
| `GET` | `/api/admin/dlp/incidents` | Recent DLP incidents (secret-free samples) |
| `GET`/`PUT` | `/api/admin/capture` | Capture policy |
| `GET`/`PUT` | `/api/admin/secondpass` | Second-pass (flywheel) policy |
| `GET`/`PUT` | `/api/admin/failover` | Gateway-wide failover defaults (`timeout_ms`, `fallback_on_auth`, `breaker`) — see [Failover policy](configuration.md#failover-policy-getput-apiadminfailover) |
| `GET` | `/api/admin/webhooks` · `POST` · `DELETE /{id}` | Alert webhook endpoints (HMAC-signed delivery) |
| `POST` | `/api/admin/dataset/export` | Export reviewed captures as a labeled JSONL training artifact (sealed at rest) |
| `GET` | `/api/admin/dataset/download?key=` | Decrypt and download a `datasets/` export artifact |
| `GET` | `/api/admin/audit` | Admin audit log |

### Provider fields

`PUT /api/admin/providers/{name}` takes `kind`, `base_url`, `enabled`,
`max_concurrency`, and four fields worth spelling out:

| Field | Meaning |
|-------|---------|
| `config` | Kind-specific structured configuration, stored as JSON. Today `vertex`, `google-speech` and `google-tts` use it, for `{"project": "...", "location": "..."}`. **A save that omits it keeps what is stored** — unlike `base_url` or `enabled`, which are replaced wholesale — so a client that knows nothing about a kind's configuration cannot erase it as a side effect of an unrelated edit. A configuration that could never serve a request is rejected on save. |
| `api_key` | Static key for the OpenAI-compatible kinds. Blank keeps the stored credential. |
| `credential_json` | A Google service-account JSON key for `vertex`, `google-speech` or `google-tts`. Blank keeps the stored credential — and, when nothing is stored, means the gateway authenticates as its own federated identity. |
| `clear_credential` | `true` removes the stored credential. Blank credential fields never do, so a client that knows nothing about credentials cannot wipe one by accident; removal takes this explicit flag. A cleared `vertex`, `google-speech` or `google-tts` provider authenticates as the pod's own federated identity from the next request. Offered for every kind: `ollama` and a keyless endpoint behind `base_url` work without a key, and for the rest, taking a leaked or revoked key out of storage is worth doing even though the provider then cannot authenticate. |

`api_key` and `credential_json` seal into the same column, so a request setting
both is rejected rather than resolved by picking one. For the same reason, a
request that sets a credential and sets `clear_credential` is rejected too.

The `provider.put` audit entry records `credential` as `set`, `cleared` or
`kept`, alongside `has_key` (whether this save supplied one), so a clear is
distinguishable from a save that left the credential alone. A clear with
nothing stored to remove is recorded as `kept`.

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
