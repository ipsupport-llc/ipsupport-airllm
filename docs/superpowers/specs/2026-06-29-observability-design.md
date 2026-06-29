# Observability (P2) — design

**Goal:** Expose Prometheus metrics (request rate + per-component latency + token/cost
+ BERT load + saturation), ship Grafana dashboards as code, and add a few
dependency-free in-console sparklines — so operators can see usage, latency by
component, and the BERT bottleneck. Public-clean throughout.

Sub-project P2 of the deploy-as-product program (P1 auth DONE · **P2 observability**
· P3 BERT-scale · P4 standalone packaging · P5 Helm/ArgoCD).

## Approach

Two surfaces, one source of truth each:
- **Heavy graphs → Grafana**, fed by a Prometheus `/metrics` endpoint on the
  gateway. Dashboards ship as JSON in the repo (datasource is a variable), so we
  reuse any Grafana and keep nothing environment-specific in git.
- **At-a-glance → the console**, fed by the existing `usage_ledger` (which already
  stores `latency_ms`, `status`, tokens, `cost_usd`, `ts`). A new hourly-bucket
  query powers a handful of zero-dependency SVG sparklines on the Dashboard. No
  Prometheus dependency in the browser; no time-series stored anywhere new.

## Metrics (`internal/metrics`)

A small package wrapping `github.com/prometheus/client_golang`: its own
`*prometheus.Registry`, the collectors below, and thin helper funcs the pipeline
calls. All metric names are prefixed `airllm_`.

| Metric | Type | Labels | Recorded where |
|--------|------|--------|----------------|
| `airllm_http_requests_total` | counter | `ingress,status` | global middleware in `ServeHTTP` |
| `airllm_http_request_duration_seconds` | histogram | `ingress` | global middleware |
| `airllm_component_duration_seconds` | histogram | `component` (`routing`\|`limits`\|`dlp`\|`provider`) | per-stage spans in the data-plane handlers |
| `airllm_tokens_total` | counter | `ingress,direction` (`prompt`\|`completion`) | `finalizeUsage` |
| `airllm_cost_usd_total` | counter | `ingress` | `finalizeUsage` |
| `airllm_rate_limited_total` | counter | `reason` (`usage_limit`\|`provider_busy`) | the two 429 sites |
| `airllm_dlp_model_requests_inflight` | gauge | — | around `dlp.ModelScan` |
| `airllm_dlp_model_duration_seconds` | histogram | — | around `dlp.ModelScan` |
| `airllm_capture_dropped` | gauge (Func) | — | reads `capturePipeline.Dropped()` |

Instrumentation points (from the codebase map):
- **Global middleware**: `Server.ServeHTTP` wraps `s.mux.ServeHTTP` with a
  status-capturing `http.ResponseWriter`; records request count + duration with
  `ingress` derived from the path (`openai` for `/v1/chat/completions`+`/v1/models`,
  `anthropic` for `/v1/messages`, `control` otherwise).
- **Per-component spans**: in `handleChatCompletions` (`dataplane.go`) and
  `handleMessages` (`messages.go`), time each of `router.Resolve` (`routing`),
  `limitDenied` (`limits`), `dlpEnforce` (`dlp`), and `runChat`/`runStream`
  (`provider`) with a `time.Now()`/`metrics.ObserveComponent(name, since)` pair.
- **Tokens/cost**: in `finalizeUsage` (`exec.go`), add `metrics.RecordUsage(ingress,
  prompt, completion, cost)` (the entry already carries ingress + tokens + cost).
- **429 counters**: `metrics.IncRateLimited("usage_limit")` at the two `limitDenied`
  branches (`dataplane.go`, `messages.go`); `metrics.IncRateLimited("provider_busy")`
  where `classifyUpstreamErr` maps `errAllBusy`→429 (`exec.go`).
- **BERT load**: wrap the `dlp.ModelScan` call (`dlp.go:268`, inside the per-message
  loop) with `metrics.DLPModelInflight.Inc()/Dec()` and observe its duration. (Counts
  per-message calls — documented.)
- **Capture drops**: a `GaugeFunc` registered in `main` reading
  `capturePipeline.Dropped()`.

The registry lives on `Server` (new field, built in `NewServer`) so handlers reach
the helpers; the `GaugeFunc`s that read external state (`Dropped`) are registered in
`main` after the pipeline exists.

### `/metrics` endpoint

`GET /metrics` served by `promhttp.HandlerFor(registry, …)`, registered in
`routes()` with **no auth wrapper**, mirroring `/healthz` and `/readyz`. It exposes
usage volume/latency (not secrets); documented as **internal-scrape only — do not
route it through the public ingress**. (In k8s, a `ServiceMonitor` scrapes it; that
is P5.)

## In-console sparklines

### Series query + endpoints

A new `usageSeries(ctx, where, args, sinceHours, bucket)` sibling to `usageWindows`
in `api_self.go`, querying `usage_ledger`:

```sql
SELECT date_trunc('hour', ts) AS bucket,
       count(*)                         AS requests,
       sum(prompt_tokens+completion_tokens) AS tokens,
       sum(cost_usd)                    AS cost_usd,
       coalesce(percentile_cont(0.5)  WITHIN GROUP (ORDER BY latency_ms), 0) AS p50_ms,
       coalesce(percentile_cont(0.95) WITHIN GROUP (ORDER BY latency_ms), 0) AS p95_ms
FROM usage_ledger
WHERE ts > now() - ($1 || ' hours')::interval   -- + optional "AND user_id = $2"
GROUP BY 1 ORDER BY 1;
```

(`idx_usage_ledger_user_ts` supports the scan.) Reuses the existing
`where`/`args` convention so it serves both scopes:
- `GET /api/usage/series` (session) — `WHERE user_id = $1`.
- `GET /api/admin/usage/series` (admin) — global.

Both return `{ "series": [ {ts, requests, tokens, cost_usd, p50_ms, p95_ms} … ] }`
over the last 24h, hourly buckets.

### UI

A dependency-free `sparkline(values, opts)` helper in `app.js` that emits an inline
`<svg><polyline>` (the file is `go:embed`-served; **no build step, no dependency**).
The Dashboard renders four sparklines from `/api/usage/series` (or
`/api/admin/usage/series` for admins): tokens/hour, cost/hour, requests/hour, and
p95 latency/hour. They sit under the existing usage cards; degrade to a "no data
yet" note on an empty series.

## Grafana as code

- `deploy/grafana/dashboards/airllm-overview.json` — panels: request rate by
  status, p50/p95 request latency, per-component latency, token & cost rate,
  rate-limited (usage_limit vs provider_busy), and **BERT inflight + scan
  duration** (the bottleneck view). The datasource is a dashboard variable
  (`${DS_PROMETHEUS}`) — no hardcoded UID — so it is portable and public-clean.
- `deploy/prometheus/prometheus.yml` — one scrape job for `app:8080/metrics`.
- `deploy/grafana/provisioning/` — a datasource provisioning file (Prometheus at
  `http://prometheus:9090`) and a dashboard provider pointing at the dashboards dir.

## Compose (`metrics` profile)

Add `prometheus` and `grafana` services under `profiles: ["metrics"]` (mirrors the
existing `bert` profile), loopback-bound:
- `prometheus` mounts `deploy/prometheus/prometheus.yml`, scrapes `app:8080/metrics`,
  published `127.0.0.1:9090`.
- `grafana` mounts `deploy/grafana/provisioning` + `dashboards`, published
  `127.0.0.1:3000`; admin password via `GF_SECURITY_ADMIN_PASSWORD` env (default
  `admin`, documented as dev-only). Up with `docker compose --profile metrics up`.

On a real deploy the cluster's existing Prometheus/Grafana are used instead (the
chart ships a `ServiceMonitor` + the same dashboard JSON in P5) — nothing here is
environment-specific.

## Dependencies

`github.com/prometheus/client_golang` (+ its transitive `client_model`,
`common`, `procfs`). Standard, well-vetted. No browser dependency (SVG is hand-rolled).

## Testing

- `internal/metrics`: collectors register without panic; helpers increment the
  expected series (verify with `prometheus/client_golang`'s `testutil`). Pure,
  unit-testable.
- `usageSeries`: extract the window→interval validation into a pure helper and unit
  test it; the bucket SQL itself is **live-verified** against compose (the project
  has no DB unit harness — same split as the capture store and the P1 user store).
- Pipeline instrumentation, `/metrics`, the series endpoints, the sparklines, and
  the Grafana/compose profile are **live-verified** by the controller: hit the
  data-plane, then confirm `/metrics` exposes moving counters, the series endpoints
  return buckets, the Dashboard renders sparklines (screenshot), and
  `--profile metrics up` brings up Prometheus scraping the target + Grafana loading
  the dashboard.

## Public-clean

No secrets, hostnames, IPs, or datasource UIDs in the repo: the dashboard datasource
is a variable; Prometheus/Grafana are dev-compose-only with documented placeholder
credentials; `app:8080` is the in-compose service name (not an environment value);
`/metrics` is documented as internal-scrape. The Grafana admin password is an env
var with a dev default, never a committed secret.

## Out of scope (P2)

- k8s `ServiceMonitor` + chart-shipped dashboards (P5).
- Tracing/spans export (OpenTelemetry) — metrics only for now.
- Alerting rules (Prometheus alert rules / Alertmanager) — dashboards only.
- Per-provider inflight gauges — saturation is covered by `rate_limited_total` +
  BERT inflight; per-provider series can come later if needed.
