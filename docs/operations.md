# Operations

## Build, test, run

```sh
make build        # build ./bin/ipsupport-airllm
make test         # unit tests (go test ./...)
make test-race    # unit tests under the race detector
make vet          # go vet ./...
make fmt          # gofmt -w .
make tidy         # go mod tidy

make run          # go run against your DATABASE_URL / REDIS_URL
make compose-up   # build + run postgres, redis, and the gateway (loopback)
make compose-down # stop and delete the compose volumes
```

CI should run `go build ./...`, `go vet ./...`, `gofmt -l` (must be empty), and
`go test -race ./...`. The web console is plain HTML/CSS/JS embedded with
`go:embed`; there is no separate front-end build step.

## Migrations

SQL migrations live in `migrations/` and are **embedded and applied
automatically on boot**, in lexicographic order, inside `store.Migrate`. To add
one, drop a new `NNNN_name.sql` file with the next number; do not edit applied
migrations. A clean re-bootstrap is `make compose-down && make compose-up`.

## Security posture

- **Bind to loopback.** On a host with a public IP, never publish the gateway on
  `0.0.0.0` and never expose Postgres or Redis (Redis has no auth). Compose binds
  all three to `127.0.0.1` and offsets the datastore ports; reach a remote box
  via an SSH tunnel or `kubectl port-forward`. Override the gateway's host
  binding with `APP_BIND` only to a specific private interface.
- **Master key.** `AIRLLM_MASTER_KEY` (base64, 32 bytes) seals provider
  credentials and capture bodies. It is **required in `prod`**; `dev` derives a
  deterministic insecure key for convenience. Generate one with
  `openssl rand -base64 32` and deliver it out of band — never commit it.
- **Session key stability.** The HMAC session signing key is derived from the
  master key via HKDF-SHA256 (`AIRLLM_SESSION_KEY` overrides it explicitly).
  Sessions survive restarts and work across replicas without any extra secret to
  manage. The key is never logged.
- **Bootstrap admin.** On first boot (`local` mode), the gateway creates one
  persistent admin user. If `AIRLLM_ADMIN_PASSWORD` is set, the password is
  stored silently and never logged. If unset, a random password is logged once
  at `WARN` and never regenerated. The bootstrap is a no-op once an admin
  account exists.
- **OIDC behind the ingress.** In `AUTH_MODE=oidc`, the gateway listens on
  `0.0.0.0:8080` behind your ingress (same as without OIDC). The IdP redirect
  and callback (`/auth/sso`, `/auth/callback`) must be reachable at the public
  URL configured in `OIDC_REDIRECT_URL`. PKCE, `state`, and `nonce` are all
  enforced; ID-token signature, `iss`, `aud`, `exp`, and `nonce` are all
  verified. Set `OIDC_ROLE_MAP` to map IdP role names to AirLLM roles if they
  differ.
- **Disabling a user.** Setting `disabled=true` on a user blocks new logins
  immediately. However, an existing session cookie remains valid until its 12-hour
  TTL expires (stateless HMAC; no per-request DB lookup). The user's API keys
  keep working until individually revoked — disabling does **not** auto-revoke
  keys. Revoke keys explicitly via **Admin → Keys** or
  `POST /api/admin/keys/{id}/revoke`.
- **Secrets stay out of git.** The repository is written public-grade: no
  secrets, English-only. `deploy/.env` is git-ignored; use
  [`deploy/.env.example`](../deploy/.env.example) as the template.
- **Redacted by default.** Capture redacts stored bodies regardless of the DLP
  action. The raw training window is the only path that stores un-redacted
  secrets, and only encrypted and TTL-bounded — see
  [DLP, capture & audit](dlp-capture-audit.md#the-raw-training-window).
- **Least privilege.** The container runs as a non-root user (`uid 10001`).

## Datastores & backups

- **Postgres** is the source of truth — back it up. It holds identity, keys,
  sealed provider credentials, the usage ledger, DLP incidents, the capture
  index, and runtime settings.
- **Redis** holds only ephemeral rolling-usage counters; it can be rebuilt and
  does not require backup.
- **Blob store** holds sealed capture bodies. In dev it is `CAPTURE_BLOB_DIR` on
  the filesystem; back it with a volume or object store on deploy. Capture
  retention and the raw-window TTL bound its growth.

## Deploy notes (kubernetes)

The default `AUTH_MODE=local` (password login) is suitable for self-hosted
deploys. Two things typically change for a production kubernetes deploy:

- **Auth:** set `AUTH_MODE=oidc` and supply the `OIDC_*` vars (issuer, client
  ID/secret, redirect URL, roles claim). Any OIDC-compliant IdP works; role
  mapping is configurable via `OIDC_ROLE_MAP`. `AUTH_MODE=local` remains fully
  supported if you prefer to manage users directly.
- **Providers:** add real providers (OpenAI, OpenRouter, xAI, Anthropic) with
  credentials entered through the console; they are sealed with the master key
  before storage and never returned or logged.

In kubernetes the app listens on `0.0.0.0:8080` behind the ingress (the
loopback rule is about not exposing an unauthenticated port directly on a host).
Run a single writer for the migration step or rely on the idempotent,
ordered-on-boot migrator.

## Observability

- `GET /healthz` — liveness. `GET /readyz` — readiness (datastore reachability).
- Logs are structured JSON (`slog`). Bootstrap and demo-user passwords (when not
  supplied via env vars) are logged once at `WARN` on first boot; treat any log
  containing credentials as dev-only. Env-provided passwords are never logged.
- The capture pipeline exposes a dropped-records counter for overload visibility.

### Prometheus metrics

`GET /metrics` returns the gateway's Prometheus counters, histograms, and
gauges in text format. It is unauthenticated and must be scraped **inside the
cluster or container network — never via the public ingress** (see
[Configuration → Metrics endpoint](configuration.md#metrics-endpoint-and-compose-profile)).

#### Metric catalog

All metrics are prefixed `airllm_`.

| Metric | Type | Labels | What it counts |
|--------|------|--------|----------------|
| `airllm_http_requests_total` | counter | `ingress`, `status` | Every HTTP request, labeled by ingress (`openai` / `anthropic` / `control`) and HTTP status code |
| `airllm_http_request_duration_seconds` | histogram | `ingress` | End-to-end request duration |
| `airllm_component_duration_seconds` | histogram | `component` | Per-stage latency: `routing`, `limits`, `dlp`, `provider` |
| `airllm_tokens_total` | counter | `ingress`, `direction` | Tokens metered; `direction` is `prompt` or `completion` |
| `airllm_cost_usd_total` | counter | `ingress` | Cost in USD metered per ingress |
| `airllm_rate_limited_total` | counter | `reason` | 429 responses: `usage_limit` (rolling window) or `provider_busy` (all targets saturated) |
| `airllm_dlp_model_requests_inflight` | gauge | — | In-flight BERT-NER sidecar scans (the saturation indicator for the DLP bottleneck) |
| `airllm_dlp_model_duration_seconds` | histogram | — | Per-message BERT scan duration |
| `airllm_capture_dropped` | gauge | — | Capture records dropped due to a full async buffer |

### Grafana dashboards

Dashboard JSON lives in `deploy/grafana/dashboards/`. The datasource is a
`${DS_PROMETHEUS}` template variable — no hardcoded UID — so the file is
portable and can be imported into any Grafana. Panels cover: request rate by
status, p50/p95 request latency, per-component latency, token and cost rate,
rate-limited breakdown, and BERT inflight + scan duration (the bottleneck view).

To bring up the full local observability stack (Prometheus + Grafana,
loopback-only):

```sh
docker compose -f deploy/docker-compose.yml --profile metrics up
```

On a real deploy, use the cluster's existing Prometheus and Grafana instead
(a `ServiceMonitor` + the same dashboard JSON ship in the Helm chart, a later
sub-project). Nothing in the repo is environment-specific.

### In-console sparklines

The Dashboard renders four sparklines (tokens/hour, cost/hour, requests/hour,
p95 latency/hour) without requiring Prometheus. They are fed by the
`usage_ledger` table via:

- `GET /api/usage/series` — current user's data (last 24 h, hourly buckets).
- `GET /api/admin/usage/series` — gateway-wide data (admin only).

## Scaling the DLP BERT sidecar

The BERT-NER model layer runs as an external sidecar and is load-balanced by a
pool (configured under **Admin → DLP**). Multiple replicas improve throughput,
and the pool distributes scans round-robin across endpoints.

### Docker Compose

```sh
docker compose --profile bert up -d --scale dlp-bert=3
```

Then set **Sidecar URL** (under **Admin → DLP**) to the service name:
`http://dlp-bert:8000`. The pool resolves the hostname to all three container
IPs and balances requests across them automatically.

Alternatively, add explicit sidecar services in `docker-compose.yml` and list
each URL in **Sidecar URLs** (one per line), e.g.:
```
http://dlp-bert-1:8000
http://dlp-bert-2:8000
http://dlp-bert-3:8000
```

### Kubernetes

Deploy `dlp-bert` as a `Deployment` with `replicas: N` behind a Kubernetes
`Service`. A standard Service uses kube-proxy load-balancing; a headless Service
(`.spec.clusterIP: None`) creates one pool endpoint per pod. Add a horizontal
pod autoscaler (HPA) on CPU utilization to scale automatically.

(The Helm chart for AirLLM ships in phase P5 and includes the `dlp-bert`
Deployment, Service, and HPA templates.)

### Monitoring saturation

Watch these `/metrics` signals to detect when the pool needs more capacity:

- `airllm_dlp_model_endpoints` — current pool size (number of resolved endpoints)
- `airllm_dlp_model_requests_inflight` — in-flight scans (saturation indicator)
- `airllm_dlp_model_duration_seconds` — per-message scan latency (histogram)
- `airllm_dlp_model_skipped_total{reason="all_busy"}` — rising when all
  endpoints are at the per-endpoint concurrency cap; the pool skips the model
  scan and only deterministic redaction applies (this is fixed fail-open behavior,
  not a setting). **If this counter is rising, scale up the pool.**
- `airllm_dlp_model_skipped_total{reason="no_endpoints"}` — rising when the
  configured sidecar URL isn't resolving; the pool has zero endpoints. **Scaling
  up will not help — fix the DNS/config problem first.**

Set **Max concurrent scans per endpoint** (under **Admin → DLP**) to cap load per
replica (0 = unlimited). When capacity is exhausted or no endpoints are reachable,
the gateway automatically skips the model scan (fail-open) and always passes the
request through with deterministic redaction applied.
