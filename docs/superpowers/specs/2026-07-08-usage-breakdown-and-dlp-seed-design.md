# Usage Breakdown by Provider/Model + DLP Sidecar URL Seeding — Design

**Date:** 2026-07-08
**Status:** approved

Two operator-reported gaps, one branch.

## Part 1 — Usage breakdown by provider and by model

### Problem

The usage views (own + admin) aggregate only hourly totals. `usage_ledger`
already records `provider_name`, `alias`, `upstream_model`, tokens, cost,
status, and latency per request — nothing surfaces which provider burns the
money or which model is slow/failing.

### Backend

Two new endpoints, same shape, same `?hours=` clamp (24 default, 168 max) as
the existing series endpoints:

```
GET /api/usage/breakdown?hours=24         — caller's own rows (session-gated)
GET /api/admin/usage/breakdown?hours=24   — all rows (admin-gated)
```

Response:

```json
{
  "providers": [
    {"provider": "openai", "requests": 120, "tokens": 84210, "cost_usd": 1.9312,
     "p95_ms": 2100, "errors": 3}
  ],
  "models": [
    {"alias": "general", "provider": "openai", "upstream_model": "gpt-5.4-mini",
     "requests": 90, "tokens": 61000, "cost_usd": 1.61, "p95_ms": 2300, "errors": 1}
  ]
}
```

One SQL query per group (shared helper, `where` clause injected exactly like
`usageSeries`):

- providers: `GROUP BY provider_name` (skip rows with empty provider_name).
- models: `GROUP BY alias, provider_name, upstream_model`.
- `errors` = `count(*) FILTER (WHERE status >= 400)`.
- `p95_ms` = `percentile_cont(0.95) WITHIN GROUP (ORDER BY latency_ms)`.
- Order by cost desc, then requests desc.

### Frontend

- Own **Usage** page and admin **usage** tab each gain two tables below the
  existing series chart: "By provider" and "By model", fed by the matching
  breakdown endpoint, refreshed together with the existing series fetch
  (same hours window control).
- Columns: Provider | Requests | Tokens | Cost | p95 | Errors, and
  Alias | Provider | Upstream model | Requests | Tokens | Cost | p95 | Errors.
- Empty state: "No traffic in this window."

## Part 2 — DLP sidecar URL seeding (kill the copy-paste junk)

### Problem

The DLP config (incl. `model_url`) lives only in the DB and is typed by hand
in the console; docs examples use the compose hostname (`http://dlp-bert:8000`),
which operators paste into k8s deployments where it is junk. The chart already
knows the correct in-cluster Service name but never tells the app.

### Fix

- New optional env `DLP_MODEL_URL_DEFAULT`. At boot, after migrations: if the
  DLP config row does not exist yet (first boot — exactly the same "seed only
  when absent" semantics as builtin roles), initialize it with
  `model_enabled: false`… — no. Precisely: seed ONLY `model_url` into the
  default DLP config row **when no DLP config row exists in the DB at all**.
  An operator who has ever saved DLP config keeps their values forever; the
  env is a first-boot default, never an override.
- Chart: when `dlpBert.enabled`, `configmap-env.yaml` sets
  `DLP_MODEL_URL_DEFAULT: http://<release>-dlp-bert Service DNS:8000` (via the
  existing helper in `_helpers.tpl`).
- Compose (`deploy/docker-compose.yml` + `deploy/compose.prod.yaml`): app env
  `DLP_MODEL_URL_DEFAULT: http://dlp-bert:8000`.
- Docs: `configuration.md` model_url row explains the default comes from the
  deployment (compose/chart) and manual entry is only for custom setups;
  remove the bare compose-hostname example from the k8s-relevant text.

### How DLP config storage works today (implementer note)

Check `internal/httpapi/api_dlp.go` / the dlpConfig load path for the exact
storage shape (single config row / kv). Seeding must follow the existing
storage mechanism; if config is stored as a row in a `config`-style table,
"absent row" is the seed condition. If the app instead falls back to compiled
defaults when no row exists, seed by writing the default struct with
`model_url` filled, `ON CONFLICT DO NOTHING`.

## Out of scope

- No per-provider Prometheus labels (cardinality; ledger covers reporting).
- No CSV export, no date-range picker beyond the existing hours window.
- No change to DLP scanning behavior, timeouts, or the pool.

## Verification

- Unit: breakdown SQL helper with fake rows is impractical (no PG harness) —
  use the TEST_DATABASE_URL-gated pattern from policy_snapshot_test.go:
  insert ledger rows in a rolled-back tx, assert grouping/error-count/order.
- Live (compose): traffic through mock alias → both breakdown endpoints
  return the mock provider row; UI tables render; playwright walk.
- Seeding: fresh DB boot with DLP_MODEL_URL_DEFAULT set → GET /api/admin/dlp
  shows the URL; boot again with a different env value → unchanged (no
  override); operator save survives restarts.
