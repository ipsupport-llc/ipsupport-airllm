# Per-Alias BERT Scan Toggle Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Aliases get a `dlp_model_scan` switch: traffic on an alias with the switch off skips the layer-2 BERT sidecar scan (layer-1 deterministic scanning unaffected).

**Architecture:** New boolean column on `model_aliases`, carried through `routing.Plan` (resolved before DLP runs on both ingress paths), gating the model-scan branch in `dlpEnforce`. Admin API + alias editor checkbox + table badge.

**Tech Stack:** Go 1.26, pgx v5, vanilla JS SPA. No new deps.

**Spec:** `docs/superpowers/specs/2026-07-11-alias-bert-toggle-design.md`

## Global Constraints

- English only; no new dependencies; no environment-specific values.
- The toggle affects ONLY the layer-2 model scan; layer-1 deterministic scanning, redaction, incidents and capture behavior are unchanged.
- Default `true` everywhere: migration default, omitted API field, passthrough plans, new-alias UI checkbox — upgrading changes nothing until an operator opts an alias out.
- A gated-off scan is configuration, NOT a skip: do not touch `airllm_dlp_model_skipped_total`.
- Migrations must replay cleanly (the migrate runner applies files in order; follow the existing file numbering/style).
- Integration tests gated on `TEST_DATABASE_URL` (compose PG at `postgres://airllm:airllm@127.0.0.1:55432/airllm?sslmode=disable`); run migrations against it first if the new column is missing (`go run ./cmd/ipsupport-airllm` is NOT needed — the test DB gets the column via the app container rebuild in live-verify; for the gated test, `ALTER TABLE` idempotently inside the tx is NOT possible — instead the test must `t.Skip` with a clear message if the column does not exist yet, and the implementer runs the app once against the dev stack to apply the migration before running the gated test).
- `gofmt -l .` clean before every commit.

---

### Task 1: Migration + routing plan flag + admin API round-trip

**Files:**
- Create: `migrations/0008_alias_dlp_model.sql`
- Modify: `internal/routing/routing.go` (Plan struct ~line 29, `Resolve` alias query ~line 106, passthrough plan ~line 102)
- Modify: `internal/httpapi/api_admin.go` — `handleAdminAliases` (SELECT + view struct, ~line 331-392) and `handleAdminPutAlias` (body + INSERT, ~line 394-431)
- Test: `internal/routing/routing_flag_test.go` (new, TEST_DATABASE_URL-gated)

**Interfaces:**
- Consumes: existing `model_aliases` table, `aliasView` struct, `Router.Resolve`.
- Produces: `Plan.DLPModelScan bool`; `aliasView.DLPModelScan bool` with json tag `dlp_model_scan`; PUT body field `dlp_model_scan *bool` (nil → true). Task 2 consumes `plan.DLPModelScan`; Task 3 consumes the API field.

- [ ] **Step 1: Migration**

Create `migrations/0008_alias_dlp_model.sql`:

```sql
-- Per-alias switch for the layer-2 (BERT) DLP model scan. Layer-1
-- deterministic scanning is not affected by this flag.
ALTER TABLE model_aliases
    ADD COLUMN dlp_model_scan boolean NOT NULL DEFAULT true;
```

Check how existing migrations are registered (embedded FS glob in `internal/store/migrate.go`) — if files are auto-globbed, the new file is picked up automatically; verify, do not hand-register unless the existing code requires it.

- [ ] **Step 2: Plan flag**

`internal/routing/routing.go`:

- `Plan` struct: add `DLPModelScan bool // run the layer-2 BERT scan for this alias` after `Strategy`.
- Passthrough branch (~line 102): `return &Plan{Alias: model, Strategy: "round_robin", DLPModelScan: true, Tiers: ...}`.
- Alias branch: change the strategy query to `SELECT strategy, dlp_model_scan FROM model_aliases WHERE alias = $1` and scan both into locals; set `Plan.DLPModelScan` when building the plan (~line 145).

- [ ] **Step 3: Admin API**

`handleAdminAliases`: add `dlp_model_scan` to the SELECT (it joins `model_aliases` — inspect the exact query first), carry it into `aliasView`:

```go
type aliasView struct {
	Alias        string        `json:"alias"`
	Protocol     string        `json:"protocol"`
	Strategy     string        `json:"strategy"`
	DLPModelScan bool          `json:"dlp_model_scan"`
	Targets      []aliasTarget `json:"targets"`
}
```

(Adjust to the real struct definition — field order/json tags must match existing style.)

`handleAdminPutAlias`: body gains `DLPModelScan *bool \`json:"dlp_model_scan"\``; resolve `scan := true; if body.DLPModelScan != nil { scan = *body.DLPModelScan }`; extend the INSERT:

```sql
INSERT INTO model_aliases (alias, protocol, strategy, dlp_model_scan) VALUES ($1, $2, $3, $4)
ON CONFLICT (alias) DO UPDATE SET protocol = EXCLUDED.protocol, strategy = EXCLUDED.strategy, dlp_model_scan = EXCLUDED.dlp_model_scan
```

- [ ] **Step 4: Gated integration test**

`internal/routing/routing_flag_test.go`: connect via TEST_DATABASE_URL (skip when unset; ALSO skip with a clear message when `SELECT dlp_model_scan FROM model_aliases LIMIT 0` errors — migration not applied yet). In a rolled-back tx: insert provider + alias with `dlp_model_scan=false` + one target; `Resolve` (construct `routing.NewRouter` over a store whose PG is the tx — if Router queries via `r.st.PG` directly, instead test the SQL semantics: select the flag as Resolve does; keep it honest but simple). Assert: alias flag false; passthrough plan (`provider/upstream`) has `DLPModelScan == true`.

Check `Router`'s store access pattern first — if `Resolve` cannot run against a tx, test at the SQL level exactly like the breakdown test does (same file comments explain why).

- [ ] **Step 5: Run + commit**

```bash
gofmt -l . && go build ./... && go vet ./... && go test ./...
# apply the migration to the dev stack DB, then:
TEST_DATABASE_URL="postgres://airllm:airllm@127.0.0.1:55432/airllm?sslmode=disable" go test ./internal/routing/ -run Flag -v
git add migrations/ internal/routing/ internal/httpapi/api_admin.go
git commit -m "feat(dlp): per-alias dlp_model_scan flag in schema, plan, and admin API"
```

To apply the migration to the dev stack: `docker compose -f deploy/docker-compose.yml up --build -d app` (the app migrates on boot) — run it before the gated test and note it in the report.

---

### Task 2: Gate the model scan in dlpEnforce (both ingress paths)

**Files:**
- Modify: `internal/httpapi/dlp.go:265` (`dlpEnforce` signature + `modelOn`, ~line 275)
- Modify: `internal/httpapi/dataplane.go:48` (openai ingress call site)
- Modify: `internal/httpapi/messages.go:46` (anthropic ingress call site)
- Test: extend an existing dlp test file (see `internal/httpapi/dlp_guardrails_test.go` for the harness pattern) with a case proving `modelScan=false` skips the model pool.

**Interfaces:**
- Consumes: `plan.DLPModelScan` (Task 1).
- Produces: `dlpEnforce(ctx, ak, ingress, req, modelScan bool)` — signature change; grep ALL callers (`grep -rn 'dlpEnforce(' --include=*.go internal`) including tests and update each.

- [ ] **Step 1: Signature + gate**

`dlp.go`: `func (s *Server) dlpEnforce(ctx context.Context, ak authedKey, ingress string, req *llm.ChatRequest, modelScan bool) (...)`; line ~275 becomes:

```go
	modelOn := cfg.ModelEnabled && modelScan && len(cfg.effectiveModelURLs()) > 0
```

Call sites: `dataplane.go:48` → `s.dlpEnforce(r.Context(), ak, "openai", &req, plan.DLPModelScan)`; `messages.go:46` → `s.dlpEnforce(r.Context(), ak, "anthropic", &req, plan.DLPModelScan)`. Note: in both handlers `plan` is resolved BEFORE the dlp call — verify and do not reorder anything.

- [ ] **Step 2: Test**

In the existing dlp test harness, add a case: server with model scanning enabled and a pool/fake that fails the test if called (or a counter that must stay 0); `dlpEnforce(..., false)` on a message that layer-1 flags — assert layer-1 findings still present, model scan not invoked. Mirror an existing test's setup (read `dlp_guardrails_test.go` first; reuse its fakes).

- [ ] **Step 3: Run + commit**

```bash
gofmt -l . && go build ./... && go vet ./... && go test ./...
git add internal/httpapi/
git commit -m "feat(dlp): honor the per-alias BERT scan flag on both ingress paths"
```

---

### Task 3: Console — checkbox + table badge

**Files:**
- Modify: `web/static/app.js` — `adminAliases` table (~line 884) and `editAlias` modal (~line 905+)

**Interfaces:**
- Consumes: `dlp_model_scan` in GET/PUT alias payloads (Task 1).
- Produces: UI only.

- [ ] **Step 1: Table badge**

In `adminAliases`, add a "BERT" column between Strategy and Targets: `a.dlp_model_scan` → `<span class="badge neutral">on</span>`, else `<span class="badge revoked">off</span>`. Update the header array accordingly.

- [ ] **Step 2: Editor checkbox**

In `editAlias` modal HTML, after the strategy `<select>` label block, add:

```html
    <label class="field"><span class="lab">Layer-2 BERT scan (fuzzy PII)</span>
      <input id="al-bert" type="checkbox" ${a.dlp_model_scan === false ? "" : "checked"} style="width:auto" /></label>
```

In the save handler, include `dlp_model_scan: $("#al-bert", bg).checked` in the PUT body.

- [ ] **Step 3: Verify + commit**

```bash
node --check web/static/app.js
git add web/static/app.js
git commit -m "feat(ui): per-alias BERT scan checkbox and badge"
```

---

### Task 4: Live verification (controller)

- [ ] Rebuild compose app (applies migration). Enable model scanning in dev DLP config with the local dlp-bert (compose profile or a stub); if no sidecar runs in the dev stack, verify via the code path: alias with scan off → `airllm_dlp_model_requests_inflight`/dlp-bert log silence; simplest oracle: dlp-bert container logs (`docker logs deploy-dlp-bert-1`) — /scan lines appear for the scan-on alias and not for the scan-off alias.
- [ ] Playwright: create alias with the checkbox off → table shows `off` badge; reopen editor → checkbox unchecked; chat via both aliases; UI walk green.
- [ ] Full e2e regression (`e2e-full.mjs` from the scratchpad) still ALL GREEN.
