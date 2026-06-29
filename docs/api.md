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
| `POST` | `/v1/chat/completions` | OpenAI Chat Completions. Non-streaming and SSE (`stream: true`). |
| `GET` | `/v1/models` | OpenAI model list, filtered by the key's allowed models |
| `POST` | `/v1/messages` | Anthropic Messages. Non-streaming and SSE. |

The `model` field accepts a configured **alias** (e.g. `mock-gpt`) or, when the
key's role allows passthrough, an explicit `provider/model`. Cross-protocol
calls are translated; same-protocol calls pass through. See
[`translation.md`](translation.md).

Errors use the caller's protocol shape (OpenAI error object vs Anthropic error
object). When every routing target is busy the gateway returns `429` rather
than failing.

## Control-plane — self-service (session)

| Method | Path | Purpose |
|--------|------|---------|
| `GET` | `/api/me` | Current principal (subject, email, roles, is_admin) |
| `POST` | `/api/me/password` | Change own password. Body: `{"current","new"}`. Local-auth users only; OIDC-provisioned users (`auth_source=oidc`) are rejected. Requires the caller's current password (not admin-only). |
| `GET` | `/api/keys` | List the caller's API keys |
| `POST` | `/api/keys` | Create a key (token returned once). Body: `{"name"}` |
| `POST` | `/api/keys/{id}/revoke` | Revoke one of the caller's keys |
| `GET` | `/api/usage` | The caller's rolling-window usage (tokens + cost) |

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
| `GET`/`PUT` | `/api/admin/roles` · `/api/admin/roles/{role}` | Role policies (allowed models, passthrough, limits) |
| `GET`/`PUT`/`DELETE` | `/api/admin/aliases` · `/api/admin/aliases/{alias}` | Model alias catalog (targets, strategy, fallback tiers) |
| `GET`/`PUT` | `/api/admin/providers` · `/api/admin/providers/{name}` | Providers (kind, base URL, sealed credential, max concurrency, enabled) |
| `GET`/`PUT` | `/api/admin/pricing` · `/api/admin/pricing/{model}` | Per-model input/output pricing (USD per 1M tokens) |
| `GET`/`PUT` | `/api/admin/dlp` | DLP policy (incl. Sensitive Info Detection patterns + custom patterns) |
| `GET` | `/api/admin/dlp/patterns` | Catalog of toggleable detection patterns (built-ins + model toggles) |
| `GET` | `/api/admin/dlp/incidents` | Recent DLP incidents (secret-free samples) |
| `GET`/`PUT` | `/api/admin/capture` | Capture policy |
| `GET`/`PUT` | `/api/admin/secondpass` | Second-pass (flywheel) policy |
| `GET` | `/api/admin/webhooks` · `POST` · `DELETE /{id}` | Alert webhook endpoints (HMAC-signed delivery) |
| `POST` | `/api/admin/dataset/export` | Export reviewed captures as a labeled JSONL training artifact |
| `GET` | `/api/admin/audit` | Admin audit log |

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
