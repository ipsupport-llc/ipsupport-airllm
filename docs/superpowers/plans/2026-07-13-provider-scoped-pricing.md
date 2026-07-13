# Provider-Scoped Pricing + Import Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Pricing rows are keyed `(provider, model)` with `provider=""` as a wildcard fallback, and an admin "Import prices" action pulls a provider's whole catalog pricing (OpenRouter publishes it) in one click.

**Architecture:** Migration 0009 re-keys the pricing table; `pricing.Table` gets a two-level lookup (exact → wildcard → 0); a new optional provider capability parses catalog pricing; one import endpoint upserts rows and refreshes the in-memory table. Chart bump 0.1.9 rides in this branch (release guard).

**Tech Stack:** Go 1.26, pgx v5, vanilla JS. No new deps.

**Spec:** `docs/superpowers/specs/2026-07-13-provider-scoped-pricing-design.md`

## Global Constraints

- English only; no new Go dependencies; no environment-specific values.
- Upgrade must be behavior-neutral: existing rows become `provider=''` wildcards and keep matching exactly as before.
- OpenRouter pricing units: `pricing.prompt`/`pricing.completion` are USD **per token** encoded as strings → store per-1M (`value * 1e6`). Entries with a missing/unparseable pricing object are SKIPPED, not zeroed.
- The import endpoint must never fail the whole import on one bad row; unknown provider → 404; provider without the capability → `{"imported": 0}` with 200.
- Old console compatibility: `PUT /api/admin/pricing/{model}` without `provider` in the body writes the wildcard row.
- Chart `version`/`appVersion` → 0.1.9 in this branch.
- `gofmt -l .` clean before every commit.

---

### Task 1: Migration + pricing.Table provider dimension + admin GET/PUT

**Files:**
- Create: `migrations/0009_pricing_provider.sql`
- Modify: `internal/pricing/pricing.go` (whole package — small)
- Modify: `internal/httpapi/exec.go:75` (cost call site)
- Modify: `internal/httpapi/api_admin.go` (`handleAdminPricing` ~474, `handleAdminPutPricing` ~499)
- Test: `internal/pricing/pricing_test.go` (new, pure unit — no DB)

**Interfaces:**
- Consumes: `entry.ProviderName` (already populated by `chatEntry` from the routing target; empty for exhausted-target failures — wildcard lookup then, which is correct).
- Produces: `Table.CostMicroUSD(provider, model string, prompt, completion int) int64`; `Table.Set(provider, model string, p Price)`; `Load` reads the provider column. API rows gain `"provider"`.

- [ ] **Step 1: Migration**

`migrations/0009_pricing_provider.sql`:

```sql
-- Pricing becomes provider-scoped. provider = '' is a wildcard row matching
-- any provider; all pre-existing rows become wildcards (upgrade-neutral).
ALTER TABLE pricing ADD COLUMN provider text NOT NULL DEFAULT '';
ALTER TABLE pricing DROP CONSTRAINT pricing_pkey;
ALTER TABLE pricing ADD PRIMARY KEY (provider, model);
```

- [ ] **Step 2: pricing package**

Key the map by a composite: `map[string]Price` with key `provider + "\x00" + model` (private `key()` helper — avoids a nested map and allocation churn). `CostMicroUSD(provider, model, ...)`: exact key, then `key("", model)`, then 0. `Set(provider, model, p)`. `Load`: `SELECT provider, model, input_per_1m, output_per_1m FROM pricing`. Update the package doc comment ("looked up by provider+model with a wildcard-provider fallback").

Write the unit test FIRST (`pricing_test.go`): table with `Set("openrouter","m",{1,2})` + `Set("","m",{10,20})` + nothing for "x" — assert exact beats wildcard, wildcard fires for another provider, unknown model = 0, and the µUSD math for a known pair (e.g. 1000 prompt + 1000 completion at {1,2} = 3000 µUSD... compute exactly: 1000/1e6*1 + 1000/1e6*2 = 0.003 USD = 3000 µUSD).

- [ ] **Step 3: call site + admin handlers**

`exec.go:75` → `s.pricing.CostMicroUSD(entry.ProviderName, upstreamModel, prompt, completion)`.

`handleAdminPricing`: `SELECT provider, model, ... ORDER BY provider, model`; row struct gains `Provider string \`json:"provider"\``.

`handleAdminPutPricing`: body gains `Provider string \`json:"provider"\`` (zero value = wildcard); upsert:

```sql
INSERT INTO pricing (provider, model, input_per_1m, output_per_1m)
VALUES ($1, $2, $3, $4)
ON CONFLICT (provider, model) DO UPDATE SET
  input_per_1m = EXCLUDED.input_per_1m, output_per_1m = EXCLUDED.output_per_1m, updated_at = now()
```

then `s.pricing.Set(body.Provider, model, ...)`; audit unchanged.

- [ ] **Step 4: Run + commit**

```bash
gofmt -l . && go build ./... && go vet ./... && go test ./...
docker compose -f deploy/docker-compose.yml up --build -d app   # applies 0009 to the dev DB
git add migrations/ internal/pricing/ internal/httpapi/
git commit -m "feat(pricing): provider-scoped prices with wildcard fallback"
```

---

### Task 2: Catalog pricing capability + import endpoint

**Files:**
- Modify: `internal/providers/provider.go` (capability + type, after `ModelLister`)
- Modify: `internal/providers/openai_compat.go` (implement; extend the existing `/models` decode)
- Modify: `internal/providers/mock.go` (fixed entry for tests)
- Create: `internal/httpapi/api_pricing_import.go`
- Modify: `internal/httpapi/api_admin.go` (route next to the pricing routes)
- Modify: `docs/api.md` (route row next to the pricing rows)
- Test: extend `internal/providers/list_models_test.go` + new `internal/httpapi/api_pricing_import_test.go`

**Interfaces:**
- Produces (in `internal/providers`):

```go
// ModelPrice is a catalog entry's price in USD per 1M tokens.
type ModelPrice struct {
	ID          string
	InputPer1M  float64
	OutputPer1M float64
}

// PricedModelLister is implemented by providers whose catalog publishes
// prices (OpenRouter). Entries without pricing are omitted.
type PricedModelLister interface {
	ListModelPricing(ctx context.Context) ([]ModelPrice, error)
}
```

- `OpenAICompat.ListModelPricing`: same `GET {base}/models` call as `ListModels`, decode `{"data":[{"id","pricing":{"prompt","completion"}}]}` where prompt/completion are strings of USD-per-token; `strconv.ParseFloat`; per-1M = ×1e6; skip entries with absent/unparseable pricing; sorted by ID. Non-2xx → `httpError`.
- `Mock.ListModelPricing`: `[]ModelPrice{{ID: "mock-gpt", InputPer1M: 1, OutputPer1M: 2}}`.
- Endpoint: `POST /api/admin/pricing/import/{provider}` — registry lookup (404 unknown), type-assert `PricedModelLister` (missing → `{"imported": 0, "unsupported": true}`), fetch with a 15 s timeout, in one pgx tx upsert every row `(provider, model)` (same upsert SQL as PUT), `s.pricing.Set` for each after commit, audit `pricing.import` with the count, respond `{"imported": N}`.

- [ ] **Step 1: tests first** — providers: httptest returning two priced entries (string values, e.g. `"0.00000075"`) + one without pricing + one with garbage pricing → exactly two `ModelPrice` with per-1M `0.75`/`4.5` etc.; non-200 → error. Handler: mock registry (Mock implements the capability) → imported 1, row visible via `GET /api/admin/pricing`... the handler test has no DB — follow the established pattern: assert the upsert SQL executed via the unreachable-pool trick is NOT viable for writes; instead make the handler testable by asserting the HTTP contract with a REAL dev-stack DB via the gated pattern (TEST_DATABASE_URL) OR test the pure parsing/assembly and leave the endpoint to live-verify. Choose the gated-integration route: `api_pricing_import_test.go` gated on TEST_DATABASE_URL, builds a minimal Server (fakeAuth admin + real store pointed at the dev DB), registers the route, imports from a registered Mock provider, asserts the pricing row exists, then DELETEs the fixture rows (unique model ids per run — `fmt.Sprintf("imp-test-%d", time.Now().UnixNano())`-style prefix is impossible for Mock's fixed ID... instead: delete `WHERE provider='mock' AND model='mock-gpt'` in t.Cleanup, and accept the shared-DB caveat noted in earlier reviews).

- [ ] **Step 2: implement** per the interface block above.

- [ ] **Step 3: Run + commit**

```bash
gofmt -l . && go build ./... && go vet ./... && go test ./...
TEST_DATABASE_URL="postgres://airllm:airllm@127.0.0.1:55432/airllm?sslmode=disable" go test ./internal/httpapi/ -run PricingImport -v
git add internal/providers/ internal/httpapi/ docs/api.md
git commit -m "feat(pricing): import provider catalog prices (OpenRouter publishes them)"
```

---

### Task 3: Console + chart bump

**Files:**
- Modify: `web/static/app.js` — `adminPricing` (~851) + its edit modal (`editPricing`, read it first)
- Modify: `deploy/helm/airllm/Chart.yaml` → 0.1.9

**Interfaces:** consumes Part 1/2 API shapes; UI only.

- [ ] **Step 1:** Pricing table gains a "Provider" column (`esc(p.provider) || "(any)"`). Edit modal gains a provider `<select>`: options = `(any)` (value "") + provider names from `GET /api/admin/providers` (fetch alongside, like `editAlias` does); include `provider` in the PUT body. Next to "New price" add an "Import prices" control: a provider `<select>` (only, no (any)) + button → `POST /api/admin/pricing/import/{provider}` → toast `Imported N prices from X` (or `X's catalog publishes no prices` when `unsupported`/0) → re-render.
- [ ] **Step 2:** Chart.yaml `version: 0.1.9`, `appVersion: "0.1.9"`.
- [ ] **Step 3:**

```bash
node --check web/static/app.js && make helm-lint
git add web/static/app.js deploy/helm/airllm/Chart.yaml
git commit -m "feat(ui): provider column + price import on the pricing tab; chart 0.1.9"
```

---

### Task 4: Live verification (controller)

- [ ] Rebuild app on the dev stack (migration 0009 applies). Existing pricing rows (if any) show `(any)`.
- [ ] Stub provider with pricing fields (extend the scratchpad stub server: add `"pricing": {"prompt": "0.000001", "completion": "0.000002"}` to its /models entries), register as a provider, Import → toast count ≥ 2, rows appear as `(stub-a, stuba-alpha)` etc.
- [ ] Chat via an alias targeting the stub → ledger row `cost_usd > 0`; usage breakdown shows the non-zero cost.
- [ ] Wildcard precedence live: PUT `('', mock-model-1)` = 10/10, PUT `('mock', mock-model-1)` = 1/2 → chat via mock alias → cost matches the exact row, not the wildcard.
- [ ] Playwright: pricing tab renders Provider column; edit modal provider select round-trips; import button works.
- [ ] Full e2e regression.
