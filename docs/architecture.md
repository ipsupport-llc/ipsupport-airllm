# Architecture

The gateway is a single Go binary that serves both planes and the console from
one listener.

```
                     ipsupport-airllm (one Go binary)

 control-plane (session cookie)            data-plane (Bearer / x-api-key)
   /auth/login · /api/* · SPA "/"            /v1/chat/completions
                                             /v1/models · /v1/messages
            │                                          │
            │                                          ▼
            │                          ┌──────────────────────────────┐
            │                          │ key-auth → role policy gate → │
            │                          │ DLP scan → rolling limits →   │
            │                          │ routing/fallback → provider → │
            │                          │ metering/ledger → capture     │
            │                          └──────────────────────────────┘
            ▼                                          │
   Postgres (durable)   Redis (rolling counters)   Blob store (sealed bodies)
```

## Components (`internal/`)

| Package | Responsibility |
|---------|----------------|
| `config` | Load + validate environment configuration; derive/seal the master key |
| `httpapi` | The mux, all handlers, both planes, request pipeline, runtime-config caches |
| `auth` | Mock password login + signed session cookies; principal/role model |
| `apikey` | Key generation, hashing, prefix/last-4 |
| `policy` | Per-role allowed-model gate |
| `routing` | Alias catalog → ordered targets (strategy + fallback tiers) |
| `providers` | Provider registry; OpenAI-compatible HTTP/SSE client; concurrency semaphores |
| `openai` / `anthropic` | Protocol codecs (parse, marshal, SSE) |
| `llm` | Protocol-neutral intermediate representation |
| `limits` | Redis rolling-window counters (check-before / increment-after) |
| `pricing` / `ledger` | Per-model pricing and the durable usage ledger |
| `secrets` | AES-256-GCM sealing of provider credentials and capture bodies |
| `dlp` | Deterministic detector (regex + entropy) and the BERT sidecar client |
| `capture` | Off-hot-path capture pipeline + the `capture_index` store |
| `blob` | Object-store abstraction (filesystem for dev) for sealed bodies |
| `secondpass` | The flywheel job: re-scan, confirm/clear, find misses |
| `dataset` | Export reviewed captures as JSONL training data |
| `webhook` | HMAC-signed alert delivery |
| `store` | Postgres access + embedded migrations |
| `seed` | Dev seed data (mock provider, aliases, demo key) |

## Request flow (data-plane)

1. **Authenticate** the API key (`Authorization: Bearer` or `x-api-key`).
2. **Policy gate** — reject models the key's role does not allow.
3. **DLP** — scan the prompt; `flag`/`redact`/`block` per policy (prompts only).
4. **Limits** — check the rolling windows (5h / 24h / 7d) before dispatch.
5. **Route** — resolve the alias to ordered targets; apply the balancing
   strategy and fall back across priority tiers; return `429` if all are busy.
6. **Translate** if the client protocol differs from the upstream's; otherwise
   pass through.
7. **Meter** — record tokens and cost to the ledger; increment the rolling
   counters.
8. **Capture** — asynchronously, off the hot path, if capture is enabled.

## Planes & auth

- **Control-plane** uses an HMAC-signed session cookie (mock) or generic OIDC
  (deploy). The SPA only ever calls `/api/*`; it never touches a datastore.
- **Data-plane** uses API keys whose role policy is snapshotted onto the key
  and automatically re-snapshotted whenever an admin edits a role or a user's
  role list (and on OIDC login, where roles come from IdP claims) — one
  lookup on the hot path, never stale after a policy change.

## Storage

- **Postgres** is the source of truth: identity, keys, role policies, providers
  (with sealed credentials), pricing, the usage ledger, DLP incidents, the
  capture index, and the `settings` table that backs runtime config.
- **Redis** holds only the rolling-window usage counters (time-bucketed).
- **Blob store** holds capture bodies, always sealed with AES-256-GCM. A
  filesystem implementation backs local dev; an object store backs deploys.

Schema migrations are embedded and applied automatically on boot, in order.

## Concurrency, balancing, fallback

- Each provider has an optional `max_concurrency` semaphore.
- A model alias lists targets across priority tiers; within a tier the strategy
  is `round_robin` or `least_busy`. Lower tiers are the fallback chain.
- When every eligible target is saturated the gateway returns `429` instead of
  failing — back-pressure, not a crash.

## Hot-reload

DLP, capture, and second-pass policy are cached behind atomic pointers and
reloaded when saved via the admin API, so policy changes take effect on the
next request or job without a restart. Provider edits rebuild the registry the
same way.

## Observability

Two surfaces, one source of truth each:

- **Prometheus `/metrics`** — `GET /metrics` exposes `airllm_*` counters,
  histograms, and gauges (unauthenticated; internal-scrape only). Per-component
  histograms (`airllm_component_duration_seconds{component=routing|limits|dlp|provider}`)
  give per-stage latency breakdowns. `airllm_dlp_model_requests_inflight` tracks
  in-flight BERT scans — the primary saturation indicator when the DLP sidecar
  is the bottleneck.

- **Grafana dashboards** — JSON in `deploy/grafana/dashboards/`, datasource
  wired via a `${DS_PROMETHEUS}` template variable (no hardcoded UID). Bring up
  the full stack locally with
  `docker compose --profile metrics up` (Prometheus at `127.0.0.1:9090`,
  Grafana at `127.0.0.1:3000`). On a real deploy the cluster's Prometheus and
  Grafana are used instead.

- **In-console sparklines** — the Dashboard page renders four inline SVG
  sparklines (tokens, cost, requests, p95 latency — hourly for the last 24 h)
  sourced from `GET /api/usage/series` (or `/api/admin/usage/series` for
  admins). These query the existing `usage_ledger` table directly — no
  Prometheus dependency in the browser.
