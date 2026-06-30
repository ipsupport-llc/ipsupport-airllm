# BERT-scale (P3) — design

**Goal:** Stop the DLP BERT sidecar from being the bottleneck. Replace the
single-URL model call with a **balanced pool** of sidecar endpoints — round-robin
with per-endpoint concurrency and graceful all-busy handling — that scales in
docker-compose (explicit URL list **and** `--scale` via DNS resolution) and in
kubernetes (Service + replicas + HPA).

Sub-project P3 of the deploy-as-product program (P1 auth · P2 observability ·
**P3 BERT-scale** · P4 standalone packaging · P5 Helm/ArgoCD).

## Background

`dlpEnforce` (`internal/httpapi/dlp.go`) calls `dlp.ModelScan(ctx, s.httpc,
cfg.ModelURL, cfg.ModelMinScore, content)` once per non-empty message — a single
URL, no concurrency bound (only a 2s timeout), and the model layer is already
**best-effort / fail-open** (on error it logs and uses the deterministic layer
only). The provider layer already has the concurrency primitive to mirror
(`providers.Entry`: `Acquire()` non-blocking slot grab, `Release()`, `Free()`).

## The pool (`internal/modelpool`)

A new package holding the balanced sidecar pool.

- **endpoint**: `{ baseURL string; sem chan struct{}; inflight atomic.Int64 }`
  with non-blocking `acquire() bool` / `release()` (the `providers.Entry` pattern,
  ~10 lines, local to the package). `baseURL` is a concrete `http://host:port`.
- **Pool**: holds an atomically-swapped `[]*endpoint`, a round-robin counter
  (`atomic.Uint64`), and a config source `func() (urls []string, maxConc int)`.
  - `Pick() (*endpoint, bool)`: starting at `rr++ % n`, walk the endpoints once
    trying `acquire()`; return the first that succeeds, or `(nil, false)` if every
    endpoint is at its cap (all busy).
  - `Scan(ctx, hc, content, minScore) ([]dlp.Finding, error)`: `Pick()`; if none →
    return `(nil, ErrAllBusy)`; else `ModelScan(ctx, hc, ep.baseURL, minScore,
    content)` then `release()`. (Inflight/duration metrics stay where they are in
    `dlpEnforce`, around this call; see Metrics.)
  - `Size() int`: current endpoint count (for the metric/gauge).

### Endpoint resolution (the "both" requirement)

The pool builds its endpoint set from the configured `model_urls` by **resolving
each URL's host**:

- Parse each configured URL into `host:port`. If `host` is already an IP →
  one endpoint `scheme://host:port`.
- If `host` is a name → `net.LookupHost(host)` → one endpoint per resolved IP
  (`scheme://ip:port`). The sidecar is plain HTTP (no virtual-hosting), so dialing
  the IP directly is correct.

This unifies every scaling mode with one mechanism:
- **Explicit compose list** — `model_urls = ["http://dlp-bert-1:8000",
  "http://dlp-bert-2:8000"]` → one endpoint each.
- **compose `--scale dlp-bert=N`** — `model_urls = ["http://dlp-bert:8000"]`,
  `dlp-bert` resolves to N container IPs → N endpoints.
- **k8s** — a normal Service resolves to one ClusterIP (kube-proxy load-balances
  across pods); a **headless** Service resolves to all pod IPs (one endpoint per
  pod). Both work; HPA scales the pods.

A background goroutine **re-resolves every 30s** (and the set swaps atomically),
so scaled-up/down replicas are picked up without a restart. On a resolve failure
for a host, the last good endpoints for that host are kept (transient DNS errors
don't empty the pool). Each endpoint's `sem` is sized to `maxConc` (`0` =
unlimited, `sem == nil`).

The config source reads the live DLP settings each resolve cycle, so changing
`model_urls`/`model_max_concurrency` via the admin API flows through on the next
cycle (same hot-reload spirit as the rest of DLP).

## Config (`dlpConfig`, admin-set via `PUT /api/admin/dlp`)

Two new fields, both optional:
- `model_urls []string` — the sidecar endpoint list. When empty, the pool falls
  back to `[model_url]` (the existing single-URL field stays for back-compat).
- `model_max_concurrency int` — per-endpoint concurrent-scan cap (`0` = unlimited).

`model_enabled` + `model_min_score` are unchanged. The admin DLP UI gains a
textarea/list for `model_urls` and a number input for `model_max_concurrency`.

## Hot path (`dlpEnforce`)

Replace the single `dlp.ModelScan(cfg.ModelURL, …)` call with `s.modelPool.Scan(…)`:

- The pool is built once in `NewServer` with a config source returning
  `(cfg.effectiveModelURLs(), cfg.ModelMaxConcurrency)` from the live DLP config,
  and its resolver goroutine started (stopped on shutdown).
- The model layer runs when `cfg.ModelEnabled` and the pool has ≥1 endpoint.
- `Scan` returning `ErrAllBusy` → **skip the model scan for this message**
  (fail-open: the deterministic layer already ran) and increment a skipped
  counter. Any other error keeps the existing fail-open behaviour (log +
  deterministic only). A successful scan merges as today.

## Metrics

Reuse P2's `airllm_dlp_model_requests_inflight` + `airllm_dlp_model_duration_seconds`
(they already bracket the model call in `dlpEnforce`). Add:
- `airllm_dlp_model_skipped_total{reason}` counter (`reason="all_busy"`) — how
  often saturation caused a skip (the signal that says "scale the pool").
- `airllm_dlp_model_endpoints` gauge (Func) — current resolved endpoint count, so
  a dashboard shows the pool size next to inflight/skips.

## Compose

The `dlp-bert` service stays (profile `bert`). Both scaling modes are documented:
- **`--scale`**: `docker compose --profile bert up -d --scale dlp-bert=3`, then set
  DLP `model_url = http://dlp-bert:8000` (single) in the console — the pool
  resolves it to the 3 replica IPs. One command.
- **explicit list**: add named instances (`dlp-bert-2`, …) and set
  `model_urls = ["http://dlp-bert:8000", "http://dlp-bert-2:8000"]`.

No compose schema change is required beyond documenting `--scale` (compose already
supports it); the model endpoints are admin-set DLP settings, not env.

## kubernetes (documented; the chart ships in P5)

- A `dlp-bert` Deployment with `replicas: N` behind a Service. Set DLP
  `model_url = http://dlp-bert:8000` (normal Service → kube-proxy LB) or use a
  headless Service for per-pod pool members.
- An HPA on CPU scales the Deployment. (Scaling on the custom
  `airllm_dlp_model_requests_inflight` metric needs prometheus-adapter — a later
  enhancement.)

## Testing

- `internal/modelpool` (pure, unit-tested with a fake resolver + a fake scan func):
  round-robin distributes across endpoints; per-endpoint cap is respected
  (`acquire` fails at the cap); all-at-cap → `Pick` returns false / `Scan` returns
  `ErrAllBusy`; `release` frees a slot; resolution builds one endpoint per IP and
  one per explicit URL; a host that fails to resolve keeps its last-good endpoints;
  `effectiveModelURLs` falls back to `[model_url]` when `model_urls` is empty.
- `dlpEnforce` wiring + the admin UI + the compose `--scale` distribution are
  **live-verified** by the controller (run `--scale dlp-bert=3`, drive traffic,
  confirm requests spread across the replicas and `airllm_dlp_model_endpoints==3`,
  and that an over-cap burst increments `..._skipped_total{all_busy}` without
  failing requests — the deterministic layer still redacts).

## Components / files

- `internal/modelpool/pool.go` (create) — `Pool`, `endpoint`, `Pick`, `Scan`,
  `Resolve`/background resolver, `ErrAllBusy`, `Size`.
- `internal/httpapi/dlp.go` (modify) — `model_urls`/`model_max_concurrency`
  fields; `effectiveModelURLs()`; `dlpEnforce` uses the pool; skipped counter.
- `internal/httpapi/server.go` + `cmd/.../main.go` (modify) — build/start/stop the
  pool; register the endpoints gauge.
- `internal/metrics/metrics.go` (modify) — `DLPModelSkipped(reason)` +
  `RegisterModelEndpoints(fn)`.
- `web/static/app.js` (modify) — DLP tab: `model_urls` + `model_max_concurrency` inputs.
- Docs: `docs/configuration.md`, `docs/dlp-capture-audit.md`, `docs/operations.md`.

## Public-clean

No hostnames/IPs/secrets in the repo: `dlp-bert`/`dlp-bert-2` are compose service
names; the model endpoints are admin-set runtime settings; the k8s guidance uses
placeholder Service names. Nothing environment-specific is committed.

## Out of scope (P3)

- The Helm chart's `dlp-bert` Deployment/Service/HPA (P5).
- Custom-metric (inflight-based) HPA via prometheus-adapter (later).
- Per-endpoint health-checking/circuit-breaking beyond skip-on-busy + keep-last-good
  on DNS failure (the 2s timeout + fail-open already bound a bad endpoint's impact).
