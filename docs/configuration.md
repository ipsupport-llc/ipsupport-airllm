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
| `AIRLLM_ADMIN_PASSWORD` | — | no | Password for the bootstrap admin. If unset, a random password is generated on first boot, written to `/tmp/airllm-bootstrap-admin-password` inside the container (never logged, since this process's logs are shipped to centralized storage), and then permanently stored — it is never regenerated. If set, the value is hashed and stored silently and **never logged**. Has no effect once an admin account already exists. |
| `AIRLLM_MASTER_KEY` | — | in `prod` | Base64-encoded **32-byte** AES key that seals provider credentials at rest. **Required when `ENV=prod`.** In `dev`, a random key is generated on first boot and persisted in the `settings` table (per install, not a shared hardcoded value) so sealed credentials survive restarts without configuration. |
| `LOOKUP_CACHE_TTL` | `30s` | no | How long an API-key or alias lookup is trusted without asking Postgres again (Go duration). The longest a revocation or alias edit made on another replica takes to apply. Must be positive. |
| `LOOKUP_CACHE_MAX_STALE` | `5m` | no | How old a cached key or alias may get while Postgres is unreachable before it stops being served (Go duration, not below `LOOKUP_CACHE_TTL`). `/readyz` also stays ready through a Postgres outage this long. |
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
| `allow_raw` | `false` | Send the un-redacted `raw_training` window to `model` for byte-aligned re-scanning. **`model` may be a third-party provider** — leave this off unless you've chosen a model you trust with real secrets; without it, second-pass always scans the (possibly redacted) main capture body |

### Failover policy (`GET/PUT /api/admin/failover`)

Gateway-wide defaults for the per-target failover options. A target's own
`options` (set per alias target in **Admin → Aliases** or the aliases API)
override them key by key; an unset key falls back to the value here. The
defaults below reproduce the behaviour from before these options existed, so
nothing changes until an operator opts in.

| Field | Default | Meaning |
|-------|---------|---------|
| `timeout_ms` | `0` | Time budget per target attempt; `0` means none. For a **streamed** chat it bounds the wait for the **first chunk** (a slow stream that has started is never cut); for a unary chat, a transcription or a speech request it bounds the **whole call**. A breach abandons the attempt and moves to the next target, exactly like a retryable error; the client sees one clean response from whichever target answers. |
| `fallback_on_auth` | `false` | Also move on when the upstream refuses the gateway's own credentials or account: HTTP `401`/`403`, and Google `PERMISSION_DENIED`, `UNAUTHENTICATED`, `FAILED_PRECONDITION` or a `BILLING_DISABLED` reason. Off, such an error fails the request as before. |

Per-target `options` is a free-form JSON object: the keys above are the ones
the gateway reads today, and any other key is stored and returned untouched.
A known key with the wrong type, a negative `timeout_ms` or a non-object is
rejected on save with `400`. For example, a voice alias whose first tier must
answer within two seconds and may fail over on an expired credential:

```json
{"priority": 0, "provider": "vertex", "upstream_model": "gemini-flash",
 "options": {"timeout_ms": 2000, "fallback_on_auth": true}}
```

Every failed attempt logs a `tier attempt failed` line with `alias`, `tier`
(the target's configured priority), `provider`, `upstream_model`, `reason` (`timeout`,
`provider_auth`, `model_not_found`, `rate_limited`, `http_<status>`, …),
`latency_ms` and, when the client sent one, `session` (the `X-Session-Id`
request header). The usage ledger records the serving `tier` (again the
configured priority, so it stays meaningful when another tier is disabled) and
the number of upstream `attempts` per request.

#### Circuit breaker

A tier that keeps failing can be quarantined so requests stop paying its
timeout. The breaker is keyed by **(alias, tier)** — the tier being the
configured priority — not by provider: the same provider behind another
alias, with its own budgets, is unaffected.

- **Opening.** A tier opens after `failures` consecutive failures, or when
  more than `error_rate` of the requests in the current `window_ms` failed
  once `min_requests` were seen in it. Only failures that say something about
  the tier count: retryable errors, timeouts, model-not-found and (with
  `fallback_on_auth`) auth refusals. A request the tier cannot serve because
  of the request itself — context too long, images or reasoning the model
  does not take, or any error that fails the request outright — is no verdict
  on the tier: it neither counts as a failure nor breaks a run of them, and
  neither does a client hanging up. The error-rate window is fixed, not
  sliding: it starts with the first request after the previous one ran out.
- **Open.** Every request skips the tier at once, without a call. When every
  tier a request could use is open it fails fast with `503`.
- **Probe.** When the cooldown runs out exactly one request — across all
  replicas — is let through; a stream counts as answered at its first chunk.
  If the probe's request turns out to be no verdict on the tier, the next
  request probes instead. Success closes the tier; failure re-opens it
  with the cooldown doubled, up to `max_cooldown_ms`. Once the tier has stayed
  closed for `stable_ms`, the next trip starts at `cooldown_ms` again.
- **Release.** `POST /api/admin/aliases/{alias}/tiers/{tier}/release` (or
  **Release** in the console's tier-health view, **Admin → Aliases → Health**)
  closes a tier by hand and forgets its history.

The breaker is **off** unless switched on, so existing aliases see no change.
Switch it on for every tier with the gateway-wide default, or per tier in a
target's options. Every knob below can be set in both places; a tier's own
value wins, then the gateway-wide one, then the built-in default. A tier with
several load-balanced targets resolves key by key: each key comes from the
first of its targets (by provider, then upstream model) that sets it.

| Key (`breaker.*`) | Default | Meaning |
|-------------------|---------|---------|
| `enabled` | `false` | Guard the tier with the breaker. |
| `failures` | `3` | Consecutive failures that open the tier. |
| `error_rate` | `0.5` | Open when more than this fraction of the window's requests failed. |
| `window_ms` | `30000` | Length of the error-rate window. |
| `min_requests` | `5` | Requests the window needs before `error_rate` applies. |
| `cooldown_ms` | `60000` | Cooldown of the first trip. |
| `max_cooldown_ms` | `1800000` | Ceiling the doubling cooldown stops at. |
| `stable_ms` | `600000` | Closed this long, the cooldown resets to `cooldown_ms`. |

```json
{"priority": 0, "provider": "vertex", "upstream_model": "gemini-flash",
 "options": {"timeout_ms": 2000, "breaker": {"enabled": true}}}
```

State is shared by every replica through Redis. If Redis cannot be reached a
replica keeps serving on its own in-memory breaker state and goes back to the
shared state when Redis answers again.

Transitions log `tier breaker opened` (with `reason`: `consecutive_failures`,
`error_rate` or `probe_failed`, `cooldown_ms` and `open_until`), `tier breaker
probe` and `tier breaker closed` (with `via`: `probe` or `manual`). Metrics:

| Metric | Labels | Meaning |
|--------|--------|---------|
| `airllm_breaker_state` | `alias`, `tier` | `0` closed, `1` open, `2` half-open (probing); read from the shared state at scrape time, for the tiers this replica has served since it started. |
| `airllm_breaker_transitions_total` | `alias`, `tier`, `to` | State changes, counted once by the replica that made them. |
| `airllm_tier_fallbacks_total` | `alias`, `from_tier`, `to_tier`, `reason` | Requests that moved past `from_tier`, served in the end by `to_tier` (`none` if nothing served them); `reason` is a failure reason or `quarantined`. |
| `airllm_tier_outcomes_total` | `alias`, `tier`, `outcome` | Attempts per tier: `success`, `failure`, `request_error` (failed because of the request, no verdict on the tier) or `quarantined` (skipped while open). |

## Provider kinds

Providers live in the `providers` table and are edited from the admin console
(**Admin → Providers**) or `PUT /api/admin/providers/{name}`. A provider row is
a **kind** (which client speaks to it), an **address**, a **credential**, and —
for kinds that need more than a URL — a structured **configuration**. Saving
one rebuilds the registry immediately; no restart.

`openai`, `openrouter`, `xai`, `groq`, `ollama` and `muse` are one
OpenAI-compatible HTTP client pointed at different addresses,
authenticating with `Authorization: Bearer <api_key>`; an explicit
`base_url` always overrides the default below. The other three are each
their own thing: `mock` answers in-process, `anthropic` has no client yet,
and `vertex` is described in full further down.

| Kind | Default address | Credential |
|------|-----------------|------------|
| `mock` | in-process, no network | none — always registered, even with no row |
| `openai` | `https://api.openai.com/v1` | `api_key` |
| `openrouter` | `https://openrouter.ai/api/v1` | `api_key` |
| `xai` | `https://api.x.ai/v1` | `api_key` |
| `groq` | `https://api.groq.com/openai/v1` | `api_key` |
| `ollama` | `http://localhost:11434/v1` | none — leave it blank and point `base_url` at the host running the daemon |
| `muse` | `https://api.meta.ai/v1` | `api_key` — Meta Model API (Muse Spark); also exposes an Anthropic-shaped `/v1/messages` surface this codebase doesn't use, since every kind here speaks OpenAI wire format upstream regardless |
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
`unsupported: true` — see its pricing note below. A catalogue publishes flat
rates only, so an import leaves any hand-entered long-prompt tier on the row
alone rather than flattening it.

### Long-prompt price tiers

Some vendors charge a second, higher pair of rates once the prompt crosses a
context threshold. Gemini 2.5 Pro is $1.25 / $10.00 per 1M tokens up to 200 000
prompt tokens and $2.50 / $15.00 above it — exactly double, input and output
both. A `tokens` price row carries that on itself, under **Admin → Pricing**:

- **Long-prompt threshold** — the prompt-token count the vendor's higher rates
  start above. `0` means the model has no tier, which is the default and what
  every existing row reads.
- **Input / Output $ / 1M above the threshold** — the second pair of rates.

Three things about how it prices, all of them the vendor's arithmetic rather
than a choice made here:

- The threshold is read off the **prompt** count, and it reprices the **whole
  call** — the output of a long prompt is billed at the high output rate too.
  It is not a blended rate applied only to the tokens past the breakpoint.
- The breakpoint itself is still the low rate, matching the vendor's "up to N
  tokens" wording; only a strictly larger prompt crosses over.
- Cost feeds the rolling `cost_usd` cap on the key from the same figure, so a
  long prompt counts against the cap at what it actually cost. Before this
  existed the cap let through twice the spend on a tiered model.

The tier belongs to the row the lookup picked, so a provider-specific row is
not lent the wildcard row's threshold. A threshold with either above-rate left
at zero is refused when you save it — it would price a long prompt at nothing,
which is worse than the flat rate it replaced — and a threshold cannot be set
on an `audio_second` or `text_char` row, which are not priced by prompt tokens
at all.

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
  stored anywhere, and this is the intended configuration in the cluster — the
  Helm chart projects that identity into the pod, off by default, under
  [Operations → Google Workload Identity Federation](operations.md#google-workload-identity-federation).
- **Paste a service-account JSON key** where no federated identity exists — on a
  plain VM, say. It is sealed at rest like any other credential. The console
  gives it a field of its own; the API-key field is not used by this kind.

A blank credential on a save *keeps* whatever is stored, so an unrelated edit
never removes a key. To return a provider that has been given an explicit key to
federated identity, open it in the console, tick **Remove the stored
credential** — offered only while one is stored — and save. The provider list
then reads `federated` again, and the next request authenticates as the pod's
own identity; the registry rebuilt by that save resolves a fresh ambient token
source rather than reusing the one minted from the removed key. Over the admin
API, the same is a `PUT` with `"clear_credential": true` (see
[API → Provider fields](api.md#provider-fields)).

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
Spelling both with the prefix is the convention. Pro charges more above 200 000
prompt tokens, which the row expresses as a
[long-prompt tier](#long-prompt-price-tiers). The rates for every id the curated
list offers, and why each is or is not offered, are listed under
[Operations → Vertex AI prices](operations.md#vertex-ai-prices).

**Thinking tokens are billed and are now counted.** Gemini 2.5 models think by
default, Google bills the thinking at the output rate, and its
OpenAI-compatible surface reports it nowhere except inside `total_tokens`. The
gateway folds that difference into the completion count on decode, so cost and
the rolling per-key caps see it; the thinking share is recorded separately —
see [Operations → Reasoning tokens](operations.md#reasoning-tokens). Two
practical consequences: a Vertex tier costs meaningfully more per request than
its unit price suggests, and a small `max_tokens` can be spent entirely on
thinking before any text is produced.

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
