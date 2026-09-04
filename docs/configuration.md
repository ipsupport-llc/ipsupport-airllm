# Configuration

The gateway is configured in two layers:

1. **Environment variables** — read once at startup (process identity, datastore
   connections, secrets). Changing them requires a restart.
2. **Runtime settings** — DLP, capture, and second-pass policy, stored in
   Postgres and editable from the admin console or API. These hot-reload; no
   restart is needed.

## Environment variables

Read by `internal/config` at startup. Invalid values fail fast.

| Variable | Default | Required | Notes |
|----------|---------|----------|-------|
| `DATABASE_URL` | — | **yes** | Postgres DSN, e.g. `postgres://airllm:airllm@host:5432/airllm?sslmode=disable` |
| `HTTP_ADDR` | `:8080` | no | Listen address. In compose this stays `:8080` inside the container; the host binding is controlled by `APP_BIND`. |
| `REDIS_URL` | `redis://localhost:6379/0` | no | Redis URL for rolling usage counters |
| `ENV` | `dev` | no | `dev` or `prod`. Used as the API-key environment tag (`air_<env>_…`). `dev` seeds mock data and a fixed demo key. |
| `AUTH_MODE` | `local` | no | `local` (DB-backed username/password) or `oidc` (generic OpenID Connect). `mock` is a **deprecated alias** for `local` — existing configs keep working but a deprecation warning is logged at startup. |
| `AIRLLM_SESSION_KEY` | — | no | Base64-encoded **32-byte** HMAC session signing key. Optional: if unset, the key is derived deterministically from `AIRLLM_MASTER_KEY` via HKDF-SHA256 (`info="airllm-session-v1"`), so sessions survive restarts and work across replicas with no extra secret to manage. Never logged. |
| `AIRLLM_ADMIN_USERNAME` | `admin` | no | Username for the bootstrap admin account created on first run. |
| `AIRLLM_ADMIN_PASSWORD` | — | no | Password for the bootstrap admin. If unset, a random password is **logged once** at `WARN` on first boot and then permanently stored — it is never regenerated. If set, the value is hashed and stored silently and **never logged**. Has no effect once an admin account already exists. |
| `AIRLLM_MASTER_KEY` | — | in `prod` | Base64-encoded **32-byte** AES key that seals provider credentials at rest. **Required when `ENV=prod`.** In `dev`, a deterministic *insecure* key is derived so sealed credentials survive restarts without configuration. |
| `CAPTURE_BLOB_DIR` | `capture-blobs` | no | Filesystem directory for the capture blob store (relative to the working directory by default). The process runs as a non-root user, so point this at a writable path (compose uses `/tmp/airllm-captures`). Back it with a volume or object store on deploy. |

### Compose-only

| Variable | Default | Notes |
|----------|---------|-------|
| `APP_BIND` | `127.0.0.1:8080` | Host interface the gateway publishes on. See [`deploy/.env.example`](../deploy/.env.example). Never set this to `0.0.0.0` on a host with a public IP. |
| `GF_SECURITY_ADMIN_PASSWORD` | `admin` | Grafana admin password when the `metrics` compose profile is active. **Change on any real deploy.** No effect unless Grafana is running. |

### OIDC settings (required when `AUTH_MODE=oidc`)

| Variable | Default | Notes |
|----------|---------|-------|
| `OIDC_ISSUER` | — | **required** | Issuer URL; discovery document fetched from `<issuer>/.well-known/openid-configuration`. Placeholder: `https://idp.example.com`. |
| `OIDC_CLIENT_ID` | — | **required** | Relying-party client ID. Placeholder: `CHANGE_ME`. |
| `OIDC_CLIENT_SECRET` | — | **required** | Relying-party client secret. Placeholder: `CHANGE_ME`. |
| `OIDC_REDIRECT_URL` | — | **required** | Callback URL registered with the IdP, e.g. `https://airllm.example.com/auth/callback`. |
| `OIDC_ROLES_CLAIM` | — | **required** | ID-token claim that carries roles. Supports a string array (`["admin","viewer"]`) or an object whose keys are role names (e.g. Zitadel's `urn:zitadel:iam:org:project:roles` map). |
| `OIDC_SCOPES` | `openid profile email` | no | Space-separated scopes to request. |
| `OIDC_ROLE_MAP` | — | no | Optional mapping from IdP role names to AirLLM roles, as a comma-separated list of `idpRole:airllmRole` pairs, e.g. `admins:airllm_admin,devs:airllm_user`. When unset, role names are used as-is and must match `airllm_admin`, `airllm_user`, or `airllm_auditor` exactly. |

Generate a production master key:

```sh
openssl rand -base64 32
```

## Metrics endpoint and compose profile

### `/metrics` endpoint

`GET /metrics` returns Prometheus text-format metrics on the same listener as
the rest of the API. It is **unauthenticated** (mirrors `/healthz` and
`/readyz`) so that the in-cluster Prometheus scraper can reach it without an
API key.

> **Internal-scrape only — do not route `/metrics` through the public
> ingress.** The endpoint exposes usage volume and latency (not secrets), but
> traffic patterns are still operator-sensitive. In kubernetes a
> `ServiceMonitor` scrapes it inside the cluster; in compose Prometheus reaches
> it over the container network.

### Compose `metrics` profile

Adds Prometheus and Grafana as loopback-bound services:

```sh
docker compose -f deploy/docker-compose.yml --profile metrics up
```

| Service | Host binding | Notes |
|---------|-------------|-------|
| `prometheus` | `127.0.0.1:9090` | Scrapes `app:8080/metrics` inside the container network |
| `grafana` | `127.0.0.1:3000` | Pre-provisioned with the AirLLM Overview dashboard |

Grafana admin credentials: username `admin`, password controlled by
`GF_SECURITY_ADMIN_PASSWORD` (default `admin` — **change on any real
deploy**). The dashboard datasource is a `${DS_PROMETHEUS}` variable so the
same JSON can be imported into any Grafana instance.

## Runtime settings

These live in the `settings` table and are edited via the admin console
(**Admin → DLP**) or the corresponding admin API endpoints. Each is cached in
an atomic pointer and reloaded on save, so changes take effect on the next
request/job without a restart.

### DLP policy (`GET/PUT /api/admin/dlp`)

| Field | Default | Meaning |
|-------|---------|---------|
| `enabled` | `true` | Master switch for request scanning |
| `action` | `redact` | `off` \| `flag` \| `redact` \| `block` — what to do on a detection |
| `scan_responses` | `false` | **Reserved and not enforced.** DLP scans prompts only by design — see [DLP, capture & audit](dlp-capture-audit.md#prompts-only). |
| `model_enabled` | `false` | Enable the BERT-NER sidecar (layer 2) |
| `model_url` | — | Sidecar URL; pre-seeded on first boot from the deployment's DLP_MODEL_URL_DEFAULT (chart/compose set it automatically) — set manually only for custom setups |
| `model_urls` | — | Array of sidecar URLs; overrides `model_url` when non-empty. A single hostname is resolved to all its A-records (one pool endpoint per IP), so `docker compose --scale` and k8s Services fan out automatically. |
| `model_min_score` | `0` | Minimum sidecar confidence to accept a span (the console pre-fills `0.5` as a suggested starting value) |
| `model_max_concurrency` | `0` | Per-endpoint cap on concurrent scans (0 = unlimited); when every endpoint is at the cap the scan is skipped and only the deterministic layer runs. |
| `model_scan_scope` | `last_user` | Which messages the model scan covers: `last_user` (each user message is scanned the turn it first appears; history is not re-scanned) or `all` |
| `model_scan_budget_ms` | `2000` | Total model-scan time budget per request; on exhaustion remaining scans fail open (skip metric reason `budget`) |
| `patterns` | `{}` | Sensitive Info Detection toggles: built-in pattern label → on/off. A label absent from the map uses its default (secrets on, PII off), so a partial map is fine. See [DLP, capture & audit](dlp-capture-audit.md#sensitive-info-detection). |
| `custom_patterns` | `[]` | Operator regexes: `[{ "label", "regex", "enabled" }]`. Validated on save (must compile, ≤ 512 chars, ≤ 50 entries). |

### Capture policy (`GET/PUT /api/admin/capture`)

| Field | Default | Meaning |
|-------|---------|---------|
| `enabled` | `false` | Master switch for traffic capture (off by default) |
| `sample_rate` | `0` | Fraction `[0,1]` of non-incident traffic to capture; incidents are always captured |
| `redact` | `true` | Redact secrets from stored bodies (redacted-by-default) |
| `retention_days` | `30` | How long capture rows + blobs are kept (clamped ≥ 1) |
| `raw_training` | `false` | Also store a short-lived **un-redacted** copy so the flywheel scans byte-aligned text. Stores real secrets (encrypted) until the TTL. |
| `raw_ttl_hours` | `24` | Lifetime of the raw copy. Clamped ≥ 1 and **≤ `retention_days × 24`** so a raw copy can never outlive its row. |

### Second-pass policy (`GET/PUT /api/admin/secondpass`)

| Field | Default | Meaning |
|-------|---------|---------|
| `enabled` | `false` | Run the off-hot-path re-scan job |
| `model` | — | Model alias the job uses to scan |
| `interval_sec` | `60` | Ticker interval (applied at start) |
| `min_score` | `0.7` | Minimum confidence to report a finding |

## Provider kinds

Providers live in the `providers` table and are edited from the admin console
(**Admin → Providers**) or `PUT /api/admin/providers/{name}`. A provider row is
a **kind** (which client speaks to it), an **address**, a **credential**, and —
for kinds that need more than a URL — a structured **configuration**. Saving
one rebuilds the registry immediately; no restart.

`openai`, `openrouter`, `xai`, `groq` and `ollama` are one OpenAI-compatible
HTTP client pointed at different addresses, authenticating with
`Authorization: Bearer <api_key>`; an explicit `base_url` always overrides the
default below. The other three are each their own thing: `mock` answers
in-process, `anthropic` has no client yet, and `vertex` is described in full
further down.

| Kind | Default address | Credential |
|------|-----------------|------------|
| `mock` | in-process, no network | none — always registered, even with no row |
| `openai` | `https://api.openai.com/v1` | `api_key` |
| `openrouter` | `https://openrouter.ai/api/v1` | `api_key` |
| `xai` | `https://api.x.ai/v1` | `api_key` |
| `groq` | `https://api.groq.com/openai/v1` | `api_key` |
| `ollama` | `http://localhost:11434/v1` | none — leave it blank and point `base_url` at the host running the daemon |
| `anthropic` | — | **no client yet**: a row of this kind is skipped when the registry is built, with a warning. Unrelated to the Anthropic-shaped `/v1/messages` *ingress*, which works with any kind. |
| `vertex` | assembled from its configuration — see below | a short-lived OAuth2 access token, consulted per request and refreshed when it expires |

Two things about the compatible kinds are worth knowing. They structurally
expose the audio endpoints whether or not the vendor implements them, so an
audio alias pointed at a chat-only vendor fails at request time rather than on
save. And their credential is captured once when the registry is built, which
is exactly why Vertex — whose token expires — is a separate kind.

A credential is sealed with `AIRLLM_MASTER_KEY` before it is stored and is
never returned by the admin API: `GET /api/admin/providers` reports only
`has_credential`. Saving with a blank credential keeps the stored one.

Importing prices (`POST /api/admin/pricing/import/{provider}`) reads the kind's
own `/models` catalogue. Every compatible kind accepts the call, but in
practice only OpenRouter publishes prices there, so the others import nothing.
`vertex` does not implement the interface at all and answers
`unsupported: true` — see its pricing note below.

### Vertex AI (`vertex`)

Google Vertex AI, reached over its OpenAI-compatible chat surface. It declares
chat, streaming and model listing only — deliberately no audio, so an audio
alias cannot resolve here.

**Configuration — two keys.** Both live in the provider's `config` object; the
console renders them as *Cloud project* and *Location*.

| Key | Required | Meaning |
|-----|----------|---------|
| `project` | yes, unless `base_url` is set | The Google Cloud project billed for the call |
| `location` | no — defaults to `global` | The Vertex location serving the request, e.g. `us-central1` |

They are stored structured rather than folded into a URL because an assembled
endpoint cannot be taken apart again, and because an operator entering them
separately cannot spell the project differently in two places. A save with
neither `project` nor `base_url` is rejected on the spot, rather than becoming
a failed request hours later.

**Address — the host rule.** The endpoint is assembled as:

```
https://<host>/v1/projects/<project>/locations/<location>/endpoints/openapi
```

`global` uses the unprefixed host `aiplatform.googleapis.com`; **every other
location prefixes its own name**, so `us-central1` is served by
`us-central1-aiplatform.googleapis.com`. An explicit `base_url` overrides the
whole assembly and is used verbatim — which is what makes the provider testable
against a local stub, and what covers a proxy or an endpoint pinned to another
API version.

**Model names carry a publisher prefix.** Vertex addresses models as
`publisher/model`, so an alias target reads `google/gemini-2.5-pro`. A bare
`gemini-2.5-pro` is accepted and qualified with `google/` on the way out, as a
defence against an easy slip — but see the pricing note below before relying on
that. The alias editor's dropdown offers a **curated** list of Gemini ids, not a
live catalogue: the compatibility surface publishes no model list, so a model
missing from the dropdown can still be typed in by hand.

**Credential — federated identity, or an explicit key.**

- **Leave it blank** and the gateway authenticates with the ambient Google
  application default credentials: the pod's own federated identity in the
  cluster, your own `gcloud` credentials on a laptop. Nothing long-lived is
  stored anywhere, and this is the intended configuration in the cluster.
- **Paste a service-account JSON key** where no federated identity exists — on a
  plain VM, say. It is sealed at rest like any other credential. The console
  gives it a field of its own; the API-key field is not used by this kind.

One direction only, today: a blank credential on a save *keeps* whatever is
stored, so a provider that has been given an explicit key cannot be returned to
federated identity from the console. Recreate it under a new name, or clear
`cred_enc` in the database.

Credential bytes that do not resolve **disable that provider**, loudly, with an
error in the log — never a silent fall back to the ambient identity, which
would run the gateway as a different principal than you configured. A provider
disabled this way never stops the gateway from starting, and the others keep
serving.

**A correct Vertex provider shows no stored credential.** Its credential column
in the provider list reads `federated`, not `none`: with federated identity
there is deliberately nothing to store, so a blank there is the healthy state
rather than a missing key.

**Prices are entered by hand, spelled the way the alias target is.** Google
publishes no machine-readable price list for these models, so importing prices
for a `vertex` provider is unsupported by design — it answers `unsupported:
true` instead of importing nothing while looking like it worked. Add the rows
under **Admin → Pricing**, and spell each model exactly as the alias target
spells it: cost is looked up by the target's `upstream_model` string, before
the publisher prefix is normalised for the wire. A target spelled
`gemini-2.5-pro` and priced as `google/gemini-2.5-pro` therefore costs nothing.
Spelling both with the prefix is the convention.

## Per-role policy

Roles (`airllm_admin`, `airllm_user`, `airllm_auditor`) carry a policy that is
snapshotted onto each API key at issue time (`GET/PUT /api/admin/roles`):

- `allowed_models` — list of permitted aliases; `*` means all.
- `allow_passthrough` — whether explicit `provider/model` passthrough is allowed.
- `limits` — rolling-window caps, shaped as `{ "tokens": {"24h": 200000}, "cost_usd": {"7d": 5} }`.
  Windows are `5h`, `24h`, `7d`; dimensions are `tokens`, `cost_usd`, and (for
  batch audio) `audio_seconds` and `tts_chars`.

Snapshots are rebuilt automatically — in the same transaction — when a role
policy or a user's role list changes, and on every OIDC login; existing keys
pick up policy edits immediately, no re-issue needed.

See the [API reference](api.md) for request/response shapes.

## Kubernetes (Helm chart)

On kubernetes the env vars above are supplied by the Helm chart
(`deploy/helm/airllm`) rather than set by hand: non-secret config comes from a
`ConfigMap` (chart `config.*` values) and sensitive values are read from an
**existing Secret** you create out-of-band (`existingSecret`). The Secret keys map
to env vars as:

| Secret key | Env var | Required |
|------------|---------|----------|
| `database-url` | `DATABASE_URL` | yes |
| `redis-url` | `REDIS_URL` | yes |
| `master-key` | `AIRLLM_MASTER_KEY` | yes |
| `session-key` | `AIRLLM_SESSION_KEY` | yes |
| `oidc-client-secret` | `OIDC_CLIENT_SECRET` | when `config.authMode=oidc` |
| `admin-password` | `AIRLLM_ADMIN_PASSWORD` | optional (else generated on first boot) |

See [Operations → Kubernetes (Helm chart)](operations.md#kubernetes-helm-chart)
for install, autoscaling (`app` HPA; `dlpBert.autoscaling.kind` = hpa/keda/none),
observability toggles, and ArgoCD.
