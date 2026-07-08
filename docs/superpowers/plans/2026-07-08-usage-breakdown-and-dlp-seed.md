# Usage Breakdown + DLP Sidecar URL Seed Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Usage views gain per-provider and per-model breakdown tables; k8s/compose installs come pre-wired with the correct DLP sidecar URL (no more hand-typed compose hostnames in prod).

**Architecture:** Two read-only aggregation endpoints over `usage_ledger` + tables in the SPA. A first-boot-only `settings('dlp')` seed row driven by a new `DLP_MODEL_URL_DEFAULT` env, wired by the chart (Service DNS helper) and compose files.

**Tech Stack:** Go 1.26 stdlib, pgx v5, vanilla JS SPA. No new deps.

**Spec:** `docs/superpowers/specs/2026-07-08-usage-breakdown-and-dlp-seed-design.md`

## Global Constraints

- English only; no new Go dependencies; no environment-specific values in the repo.
- Breakdown endpoints reuse the existing `clampHours` semantics (default 24, cap 168).
- The DLP seed NEVER overrides an existing `settings` row `name='dlp'` (`ON CONFLICT (name) DO NOTHING`); it seeds ONLY when the row is absent, and only when `DLP_MODEL_URL_DEFAULT` is non-empty.
- No change to DLP scanning behavior, timeouts, or the pool.
- Integration tests gated on `TEST_DATABASE_URL` (skip when unset); compose PG runs at `postgres://airllm:airllm@127.0.0.1:55432/airllm?sslmode=disable`.
- `gofmt -l .` clean before every commit.

---

### Task 1: Breakdown endpoints (`/api/usage/breakdown` + `/api/admin/usage/breakdown`)

**Files:**
- Create: `internal/httpapi/api_usage_breakdown.go`
- Modify: `internal/httpapi/server.go:212` (register own-usage route next to `GET /api/usage/series`)
- Modify: `internal/httpapi/api_admin.go:24` (register admin route next to `GET /api/admin/usage/series`)
- Modify: `docs/api.md` (two rows: one in the self-service section next to `/api/usage/series`, one in the admin section next to `/api/admin/usage/series` — match the tables' existing format)
- Test: `internal/httpapi/api_usage_breakdown_test.go` (new, TEST_DATABASE_URL-gated)

**Interfaces:**
- Consumes: `clampHours` (api_self.go:193), `sessionFrom`, `writeJSON`, `writeControlError`, `s.st.PG`, `usage_ledger` columns (`ts, user_id, provider_name, alias, upstream_model, prompt_tokens, completion_tokens, cost_usd, status, latency_ms` — verify exact names in `migrations/0001_init.sql` / `internal/ledger` before writing SQL).
- Produces: `GET /api/usage/breakdown?hours=N` (session) and `GET /api/admin/usage/breakdown?hours=N` (admin), both returning `{"providers":[...],"models":[...]}` per the spec. Task 2's UI consumes them.

- [ ] **Step 1: Implementation**

Create `internal/httpapi/api_usage_breakdown.go`:

```go
package httpapi

import (
	"context"
	"net/http"
)

type providerUsage struct {
	Provider string  `json:"provider"`
	Requests int64   `json:"requests"`
	Tokens   int64   `json:"tokens"`
	CostUSD  float64 `json:"cost_usd"`
	P95ms    int64   `json:"p95_ms"`
	Errors   int64   `json:"errors"`
}

type modelUsage struct {
	Alias         string  `json:"alias"`
	Provider      string  `json:"provider"`
	UpstreamModel string  `json:"upstream_model"`
	Requests      int64   `json:"requests"`
	Tokens        int64   `json:"tokens"`
	CostUSD       float64 `json:"cost_usd"`
	P95ms         int64   `json:"p95_ms"`
	Errors        int64   `json:"errors"`
}

// usageBreakdown aggregates the ledger by provider and by model over the last
// `hours`. where is an optional "AND user_id = $2" clause bound after $1,
// mirroring usageSeries.
func (s *Server) usageBreakdown(ctx context.Context, where string, hours int, whereArgs ...any) ([]providerUsage, []modelUsage, error) {
	args := append([]any{hours}, whereArgs...)

	provQ := `
		SELECT provider_name,
		       count(*),
		       COALESCE(SUM(prompt_tokens + completion_tokens), 0),
		       COALESCE(SUM(cost_usd), 0),
		       COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY latency_ms), 0)::bigint,
		       count(*) FILTER (WHERE status >= 400)
		FROM usage_ledger
		WHERE ts > now() - make_interval(hours => $1) AND provider_name <> '' ` + where + `
		GROUP BY provider_name
		ORDER BY 4 DESC, 2 DESC`
	rows, err := s.st.PG.Query(ctx, provQ, args...)
	if err != nil {
		return nil, nil, err
	}
	provs := []providerUsage{}
	for rows.Next() {
		var p providerUsage
		if err := rows.Scan(&p.Provider, &p.Requests, &p.Tokens, &p.CostUSD, &p.P95ms, &p.Errors); err != nil {
			rows.Close()
			return nil, nil, err
		}
		provs = append(provs, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	modelQ := `
		SELECT alias, provider_name, upstream_model,
		       count(*),
		       COALESCE(SUM(prompt_tokens + completion_tokens), 0),
		       COALESCE(SUM(cost_usd), 0),
		       COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY latency_ms), 0)::bigint,
		       count(*) FILTER (WHERE status >= 400)
		FROM usage_ledger
		WHERE ts > now() - make_interval(hours => $1) AND provider_name <> '' ` + where + `
		GROUP BY alias, provider_name, upstream_model
		ORDER BY 6 DESC, 4 DESC`
	rows, err = s.st.PG.Query(ctx, modelQ, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	models := []modelUsage{}
	for rows.Next() {
		var m modelUsage
		if err := rows.Scan(&m.Alias, &m.Provider, &m.UpstreamModel, &m.Requests, &m.Tokens, &m.CostUSD, &m.P95ms, &m.Errors); err != nil {
			return nil, nil, err
		}
		models = append(models, m)
	}
	return provs, models, rows.Err()
}

// handleUsageBreakdown returns the caller's usage grouped by provider/model.
func (s *Server) handleUsageBreakdown(w http.ResponseWriter, r *http.Request) {
	sess, _ := sessionFrom(r.Context())
	hours := clampHours(r.URL.Query().Get("hours"))
	provs, models, err := s.usageBreakdown(r.Context(), `AND user_id = $2`, hours, sess.userID)
	if err != nil {
		writeControlError(w, http.StatusInternalServerError, "failed to load usage breakdown")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": provs, "models": models})
}

// handleAdminUsageBreakdown returns global usage grouped by provider/model.
func (s *Server) handleAdminUsageBreakdown(w http.ResponseWriter, r *http.Request) {
	hours := clampHours(r.URL.Query().Get("hours"))
	provs, models, err := s.usageBreakdown(r.Context(), "", hours)
	if err != nil {
		writeControlError(w, http.StatusInternalServerError, "failed to load usage breakdown")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": provs, "models": models})
}
```

Before finalizing, verify the ledger column names against `migrations/0001_init.sql` (table `usage_ledger`) and adjust if they differ.

Register routes:
- `internal/httpapi/server.go` next to `GET /api/usage/series` (line ~212): `s.mux.HandleFunc("GET /api/usage/breakdown", s.requireSession(s.handleUsageBreakdown))`
- `internal/httpapi/api_admin.go` next to admin usage series (line ~24): `s.mux.HandleFunc("GET /api/admin/usage/breakdown", a(s.handleAdminUsageBreakdown))`

- [ ] **Step 2: Integration test**

Create `internal/httpapi/api_usage_breakdown_test.go` following the store test pattern (`internal/store/policy_snapshot_test.go`): skip without `TEST_DATABASE_URL`; open a pgxpool; run inside a rolled-back tx. Because `usageBreakdown` queries `s.st.PG` (the pool, not a tx), structure the test at the SQL level instead: execute the two query strings directly against the tx after inserting fixture rows. Fixtures: 3 rows provider `bp-openai` (one with `status=500`, distinct latencies/costs), 1 row provider `bp-mock` other alias, 1 row with empty `provider_name` (must be excluded). Assert: provider grouping (2 groups), error count 1 for `bp-openai`, empty-provider row excluded, model grouping keys, cost-desc order. Keep the query strings in the test imported from the source by exporting them as package-level `const` in the implementation file (e.g. `breakdownProviderQuery`, `breakdownModelQuery`) so the test cannot drift from the code.

- [ ] **Step 3: Run**

```bash
gofmt -l . && go build ./... && go vet ./... && go test ./internal/httpapi/
TEST_DATABASE_URL="postgres://airllm:airllm@127.0.0.1:55432/airllm?sslmode=disable" go test ./internal/httpapi/ -run Breakdown -v
```
Both must pass (second one must NOT skip; report BLOCKED if it cannot connect).

- [ ] **Step 4: Commit**

```bash
git add internal/httpapi/ docs/api.md
git commit -m "feat(usage): per-provider and per-model breakdown endpoints"
```

---

### Task 2: Breakdown tables in the SPA (own Usage page + admin usage tab)

**Files:**
- Modify: `web/static/app.js` — `viewUsage` (line ~256) and `adminUsage` (line ~672)

**Interfaces:**
- Consumes: `GET /api/usage/breakdown` / `GET /api/admin/usage/breakdown` (Task 1), existing `api()`, `esc()`, `panelTable(...)` helper (see `adminAliases` for usage), whatever hours-window control the pages already have (reuse its value; if a page has no control, default to 24h).
- Produces: user-visible tables only.

- [ ] **Step 1: Implement**

Read `viewUsage` and `adminUsage` first to match their structure. Add to each page, below the existing content, two panels rendered from the breakdown response:

```js
function breakdownTables(d) {
  const provRows = (d.providers || []).map((p) => `<tr>
    <td>${esc(p.provider)}</td><td>${p.requests}</td><td>${p.tokens}</td>
    <td>$${(+p.cost_usd).toFixed(4)}</td><td>${p.p95_ms} ms</td><td>${p.errors}</td></tr>`);
  const modelRows = (d.models || []).map((m) => `<tr>
    <td class="mono">${esc(m.alias)}</td><td>${esc(m.provider)}</td><td class="mono">${esc(m.upstream_model)}</td>
    <td>${m.requests}</td><td>${m.tokens}</td><td>$${(+m.cost_usd).toFixed(4)}</td>
    <td>${m.p95_ms} ms</td><td>${m.errors}</td></tr>`);
  const empty = `<div class="empty">No traffic in this window.</div>`;
  return (provRows.length
    ? panelTable("By provider", ["Provider", "Requests", "Tokens", "Cost", "p95", "Errors"], provRows)
    : `<div class="panel"><div class="panel-head"><h2>By provider</h2></div>${empty}</div>`)
    + (modelRows.length
    ? panelTable("By model", ["Alias", "Provider", "Upstream model", "Requests", "Tokens", "Cost", "p95", "Errors"], modelRows)
    : `<div class="panel"><div class="panel-head"><h2>By model</h2></div>${empty}</div>`);
}
```

(Define it once at top level near other helpers, reuse from both pages.) In `viewUsage`, fetch `/api/usage/breakdown?hours=<same window as the page>` alongside the existing usage fetches and append `breakdownTables(r.data)` into the page container. In `adminUsage`, same with `/api/admin/usage/breakdown`. Check `panelTable`'s signature (title, headers, rows) in app.js before use — it takes rows as an array of `<tr>` strings (see `adminAliases`).

- [ ] **Step 2: Verify + commit**

```bash
node --check web/static/app.js
git add web/static/app.js
git commit -m "feat(ui): usage breakdown tables by provider and by model"
```

---

### Task 3: `DLP_MODEL_URL_DEFAULT` seed + chart/compose wiring + docs

**Files:**
- Create: `internal/seed/dlp.go`
- Modify: `cmd/ipsupport-airllm/main.go` (call right after `seed.EnsureBuiltinRoles`)
- Modify: `deploy/helm/airllm/templates/configmap-env.yaml` (conditional env when `.Values.dlpBert.enabled`)
- Modify: `deploy/docker-compose.yml` (app env) and `deploy/compose.prod.yaml` (app env)
- Modify: `docs/configuration.md:101` (model_url row: default comes from the deployment; manual entry for custom setups only)
- Test: `internal/seed/dlp_test.go` (TEST_DATABASE_URL-gated)

**Interfaces:**
- Consumes: `settings` table (`name text PK, value jsonb`) — DLP config is row `name='dlp'`; `loadDLP` (internal/httpapi/dlp.go:211) unmarshals the stored JSON over `defaultDLPConfig()`, so a partial `{"model_url": "..."}` row keeps every other default. Chart helper `airllm.dlpBertServiceName` (deploy/helm/airllm/templates/_helpers.tpl:39).
- Produces: `seed.EnsureDLPDefaults(ctx context.Context, st *store.Store, modelURL string) error`.

- [ ] **Step 1: Implement the seed**

Create `internal/seed/dlp.go`:

```go
package seed

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/store"
)

// EnsureDLPDefaults seeds the DLP settings row with a deployment-provided
// sidecar URL on FIRST boot only. The stored JSON is partial: loadDLP
// unmarshals it over the compiled defaults, so only model_url is pinned.
// An existing 'dlp' row — i.e. any operator save — is never touched.
func EnsureDLPDefaults(ctx context.Context, st *store.Store, modelURL string) error {
	if modelURL == "" {
		return nil
	}
	val, _ := json.Marshal(map[string]string{"model_url": modelURL})
	if _, err := st.PG.Exec(ctx, `
		INSERT INTO settings (name, value) VALUES ('dlp', $1)
		ON CONFLICT (name) DO NOTHING`, val); err != nil {
		return fmt.Errorf("seed dlp defaults: %w", err)
	}
	return nil
}
```

In `cmd/ipsupport-airllm/main.go`, immediately after the `EnsureBuiltinRoles` block:

```go
	if err := seed.EnsureDLPDefaults(ctx, st, os.Getenv("DLP_MODEL_URL_DEFAULT")); err != nil {
		return fmt.Errorf("ensure dlp defaults: %w", err)
	}
```

(`os` is already imported in main.go.)

- [ ] **Step 2: Wire the deployments**

`deploy/helm/airllm/templates/configmap-env.yaml` — after the `CAPTURE_BLOB_DIR` line:

```yaml
{{- if .Values.dlpBert.enabled }}
  DLP_MODEL_URL_DEFAULT: {{ printf "http://%s:%d" (include "airllm.dlpBertServiceName" .) (int .Values.dlpBert.service.port) | quote }}
{{- end }}
```

Check `values.yaml` for the dlpBert service port key (`dlpBert.service.port`; verify the exact path, adjust if it differs).

`deploy/docker-compose.yml` and `deploy/compose.prod.yaml` — in the app service `environment`, add (with a one-line comment matching file style):

```yaml
      # First-boot default for the DLP sidecar URL; ignored once DLP config is saved.
      DLP_MODEL_URL_DEFAULT: "http://dlp-bert:8000"
```

`docs/configuration.md:101` — replace the model_url example cell with: `Sidecar URL; pre-seeded on first boot from the deployment's DLP_MODEL_URL_DEFAULT (chart/compose set it automatically) — set manually only for custom setups`.

- [ ] **Step 3: Test**

Create `internal/seed/dlp_test.go` (gated, rolled-back tx pattern from `internal/store/policy_snapshot_test.go`, but note `EnsureDLPDefaults` takes `*store.Store` — give it the same treatment as the breakdown test: exercise the INSERT semantics at SQL level in a tx, or refactor `EnsureDLPDefaults` to accept a `store.Querier` (preferred — `EnsureBuiltinRoles` can stay as-is). With Querier: begin tx → `EnsureDLPDefaults(ctx, tx, "http://svc:8000")` → row exists with only model_url key → call again with different URL → unchanged → insert a fake operator row for name='dlp' first in a fresh savepoint → seed does not clobber. Also `EnsureDLPDefaults(ctx, tx, "")` → no row created.

- [ ] **Step 4: Run + render + commit**

```bash
gofmt -l . && go build ./... && go vet ./... && go test ./...
TEST_DATABASE_URL="postgres://airllm:airllm@127.0.0.1:55432/airllm?sslmode=disable" go test ./internal/seed/ -run DLP -v
make helm-lint
docker compose -f deploy/docker-compose.yml config >/dev/null && docker compose -f deploy/compose.prod.yaml config >/dev/null
helm template deploy/helm/airllm --set existingSecret=x | grep DLP_MODEL_URL_DEFAULT   # present (dlpBert on by default)
git add internal/seed/ cmd/ipsupport-airllm/main.go deploy/ docs/configuration.md
git commit -m "feat(dlp): seed sidecar URL from DLP_MODEL_URL_DEFAULT on first boot"
```

---

### Task 4: Live verification (controller)

- [ ] Rebuild compose app; drive traffic through alias `mock-gpt` (a couple of chats incl. one with model `mock-fail-x` via passthrough for an error row).
- [ ] `GET /api/usage/breakdown` (dev key session) and `/api/admin/usage/breakdown` return provider `mock` with matching counts; error row counted.
- [ ] Playwright: Usage page and admin usage tab render both tables with the mock rows; empty-window state renders when hours=1 after no traffic (optional).
- [ ] Fresh-DB seed check: one-off container with `DLP_MODEL_URL_DEFAULT=http://airllm-dlp-bert:8000` against a fresh DB → `GET /api/admin/dlp` shows that model_url; restart with a different env value → unchanged.
