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
| `AUTH_MODE` | `mock` | no | `mock` (password login, random per-boot credentials) or `oidc` (generic OIDC, used on the kubernetes deploy) |
| `AIRLLM_MASTER_KEY` | — | in `prod` | Base64-encoded **32-byte** AES key that seals provider credentials at rest. **Required when `ENV=prod`.** In `dev`, a deterministic *insecure* key is derived so sealed credentials survive restarts without configuration. |
| `CAPTURE_BLOB_DIR` | `capture-blobs` | no | Filesystem directory for the capture blob store (relative to the working directory by default). The process runs as a non-root user, so point this at a writable path (compose uses `/tmp/airllm-captures`). Back it with a volume or object store on deploy. |

### Compose-only

| Variable | Default | Notes |
|----------|---------|-------|
| `APP_BIND` | `127.0.0.1:8080` | Host interface the gateway publishes on. See [`deploy/.env.example`](../deploy/.env.example). Never set this to `0.0.0.0` on a host with a public IP. |

Generate a production master key:

```sh
openssl rand -base64 32
```

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
| `model_url` | — | Sidecar URL, e.g. `http://dlp-bert:8000` |
| `model_min_score` | `0` | Minimum sidecar confidence to accept a span (the console pre-fills `0.5` as a suggested starting value) |

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

## Per-role policy

Roles (`airllm_admin`, `airllm_user`, `airllm_auditor`) carry a policy that is
snapshotted onto each API key at issue time (`GET/PUT /api/admin/roles`):

- `allowed_models` — list of permitted aliases; `*` means all.
- `allow_passthrough` — whether explicit `provider/model` passthrough is allowed.
- `limits` — rolling-window caps, shaped as `{ "tokens": {"24h": 200000}, "cost_usd": {"7d": 5} }`.
  Windows are `5h`, `24h`, `7d`; dimensions are `tokens` and `cost_usd`.

See the [API reference](api.md) for request/response shapes.
