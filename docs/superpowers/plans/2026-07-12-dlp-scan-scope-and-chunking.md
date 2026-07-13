# DLP Scan Scope + Budget + Sidecar Chunking Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Long-context clients stop paying N×2 s DLP tax (scope: scan only the last user message by default; per-request budget), and the sidecar stops silently truncating at 512 tokens (sliding-window chunking + explicit cap).

**Architecture:** Two new `dlpConfig` fields (`model_scan_scope`, `model_scan_budget_ms`) threaded through `dlpEnforce`; the per-message 2 s timeout becomes one per-request budget context. Sidecar `app.py` scans in overlapping windows with a hard char cap. Console DLP tab grows two controls. Chart `version`/`appVersion` → 0.1.8 in this branch (release guard requires it).

**Tech Stack:** Go 1.26, vanilla JS, python transformers sidecar. No new Go deps.

**Spec:** `docs/superpowers/specs/2026-07-12-dlp-scan-scope-and-chunking-design.md`

## Global Constraints

- English only; no new Go dependencies; no environment-specific values.
- Layer-1 deterministic scanning still runs on EVERY message — scope/budget touch ONLY the model scan.
- Scope default is `last_user`; stored configs without the field (or with an unknown value) normalize to `last_user`. Budget default 2000 ms; `<= 0` normalizes to 2000.
- New skip-metric reason string is exactly `budget`; existing reasons (`all_busy`, `no_endpoints`) unchanged.
- Sidecar response stays backward-compatible: `{"findings": [...]}` plus optional `"truncated": true` — the Go client must tolerate (ignore) the extra field (verify how it decodes).
- Chart bump to 0.1.8 (version + appVersion) is part of this branch — the release workflow guard fails the tag otherwise.
- `gofmt -l .` clean before every commit.

---

### Task 1: Gateway — scope + budget in dlpConfig and dlpEnforce (+ unit tests)

**Files:**
- Modify: `internal/httpapi/dlp.go` — `dlpConfig` struct (~line 22), `defaultDLPConfig` (~line 181), `loadDLP` normalization (~line 211), `dlpEnforce` (~line 267-320)
- Test: extend `internal/httpapi/dlp_guardrails_test.go` (reuse the `TestDlpEnforceModelScanGate` harness from the per-alias toggle work)

**Interfaces:**
- Consumes: existing `dlpEnforce(ctx, ak, ingress, req, modelScan bool)`, `modelpool.Scan`, metrics `DLPModelSkipped(reason)`.
- Produces: `dlpConfig.ModelScanScope string` (json `model_scan_scope,omitempty`), `dlpConfig.ModelScanBudgetMS int` (json `model_scan_budget_ms,omitempty`); Task 2's UI round-trips them. No signature changes.

- [ ] **Step 1: Config fields + normalization**

`dlpConfig` (after `ModelMaxConcurrency`):

```go
	// ModelScanScope selects which request messages the layer-2 model scan
	// covers: "last_user" (default — clients resend history every turn, each
	// user message is scanned when it first appears) or "all". Layer-1
	// deterministic scanning always covers every message.
	ModelScanScope string `json:"model_scan_scope,omitempty"`
	// ModelScanBudgetMS bounds the TOTAL model-scan time per request (all
	// messages combined). 0 or negative = default 2000.
	ModelScanBudgetMS int `json:"model_scan_budget_ms,omitempty"`
```

`defaultDLPConfig()` → add `ModelScanScope: "last_user", ModelScanBudgetMS: 2000`.

In `loadDLP`, after the existing action validation, normalize:

```go
	if cfg.ModelScanScope != "all" {
		cfg.ModelScanScope = "last_user"
	}
	if cfg.ModelScanBudgetMS <= 0 {
		cfg.ModelScanBudgetMS = 2000
	}
```

- [ ] **Step 2: dlpEnforce — scope index + budget context**

Before the message loop (after `modelOn` is computed):

```go
	// Scope: which messages the model scan covers. Layer-1 always scans all.
	lastUser := -1
	if modelOn && cfg.ModelScanScope != "all" {
		for i := len(req.Messages) - 1; i >= 0; i-- {
			if req.Messages[i].Role == "user" {
				lastUser = i
				break
			}
		}
	}
	// Budget: one deadline for ALL model scans in this request (replaces the
	// old per-message 2s timeout, which multiplied by history length).
	var bctx context.Context
	var bcancel context.CancelFunc
	if modelOn {
		bctx, bcancel = context.WithTimeout(ctx, time.Duration(cfg.ModelScanBudgetMS)*time.Millisecond)
		defer bcancel()
	}
```

Inside the loop, replace the per-message scan block: the condition becomes
`if modelOn && (cfg.ModelScanScope == "all" || i == lastUser)`; the scan uses
`bctx` (no per-message context); the error handling gains a budget branch
BEFORE the generic one:

```go
			mf, err := s.modelPool.Scan(bctx, s.httpc, content, cfg.ModelMinScore)
			switch {
			case errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil:
				// The request is alive but the scan budget is spent: fail open
				// for this and all remaining messages, visibly.
				s.metrics.DLPModelSkipped("budget")
			case errors.Is(err, modelpool.ErrAllBusy):
				...existing...
```

(Keep `DLPModelObserve` on the success and generic-error paths exactly as
today; do NOT observe latency for budget skips. Check `req.Messages[i].Role`
field name against `internal/llm` before coding.)

- [ ] **Step 3: Unit tests**

Extend the existing harness (fake `*Server` + httptest sidecar with a hit
counter). New test `TestDlpEnforceScopeAndBudget` with subtests:

1. `scope last_user`: request = [system, user("secret sk-... A"), assistant, user("plain B"), assistant] — exactly ONE sidecar hit, and the hit body contains "plain B"'s content (the LAST user message, not the first, not the assistant tail). Config: scope unset (normalization → last_user).
2. `scope all`: same request → one hit per non-empty message (5).
3. `budget`: scope all, sidecar handler sleeps 300 ms per hit, budget 400 ms, 4-message request → sidecar hits < 4 and the call returns without error (fail-open); layer-1 finding on a planted secret still present.

Follow the existing test's fake-server/pool construction verbatim (IP-literal
URL, StartModelPool not needed — check how the toggle test wires the pool).

- [ ] **Step 4: Run + commit**

```bash
gofmt -l . && go build ./... && go vet ./... && go test ./... && go test -race ./internal/httpapi/ -run 'DlpEnforce'
git add internal/httpapi/
git commit -m "feat(dlp): model-scan scope (last_user default) and per-request budget"
```

---

### Task 2: Console + docs + chart bump

**Files:**
- Modify: `web/static/app.js` — `adminDLP` (~line 1027, the model sidecar section) and `gatherDLP` (~line 1129)
- Modify: `docs/configuration.md` (DLP table: two new rows near `model_url`)
- Modify: `docs/dlp-capture-audit.md` (scope semantics + budget paragraph in the layer-2 section)
- Modify: `deploy/helm/airllm/Chart.yaml` — `version: 0.1.8`, `appVersion: "0.1.8"`

**Interfaces:**
- Consumes: `model_scan_scope` / `model_scan_budget_ms` (Task 1).
- Produces: UI + docs only.

- [ ] **Step 1: UI**

In `adminDLP`, after the max-concurrency field (find `#dlp-mconc`), add:

```html
        <label class="field"><span class="lab">Model scan scope</span>
          <select id="dlp-mscope">
            <option value="last_user" ${d.model_scan_scope !== "all" ? "selected" : ""}>last user message (default)</option>
            <option value="all" ${d.model_scan_scope === "all" ? "selected" : ""}>all messages</option>
          </select></label>
        <label class="field"><span class="lab">Model scan budget per request (ms)</span>
          <input id="dlp-mbudget" type="number" min="100" value="${Number(d.model_scan_budget_ms) || 2000}" /></label>
```

In `gatherDLP`, add `model_scan_scope: $("#dlp-mscope").value, model_scan_budget_ms: Number($("#dlp-mbudget").value) || 2000,`.

(Adapt markup to the exact surrounding style after reading the function.)

- [ ] **Step 2: Docs**

`configuration.md` DLP table, after the `model_max_concurrency` row:

```markdown
| `model_scan_scope` | `last_user` | Which messages the model scan covers: `last_user` (each user message is scanned the turn it first appears; history is not re-scanned) or `all` |
| `model_scan_budget_ms` | `2000` | Total model-scan time budget per request; on exhaustion remaining scans fail open (skip metric reason `budget`) |
```

`dlp-capture-audit.md`: in the layer-2/BERT section, a short paragraph: scope
default and rationale (history resent every turn; layer-1 still covers every
message), the per-request budget, and the sidecar's chunked scanning with the
`DLP_MAX_CHARS` cap.

- [ ] **Step 3: Chart bump + verify + commit**

Chart.yaml → `version: 0.1.8`, `appVersion: "0.1.8"`.

```bash
node --check web/static/app.js && make helm-lint && make check-links
git add web/static/app.js docs/ deploy/helm/airllm/Chart.yaml
git commit -m "feat(ui): DLP scan scope + budget controls; docs; chart 0.1.8"
```

---

### Task 3: Sidecar — sliding-window chunking + char cap

**Files:**
- Modify: `deploy/dlp-bert/app.py`
- Modify: `deploy/dlp-bert/Dockerfile` ONLY if a new env default needs declaring (prefer app-side default; do not add packages)

**Interfaces:**
- Consumes: POST /scan `{"text": ...}` as today.
- Produces: `{"findings": [...], "truncated": bool}` — `truncated` present (false/true) is fine as long as `findings` shape is unchanged.

- [ ] **Step 1: Implement**

```python
MAX_CHARS = int(os.environ.get("DLP_MAX_CHARS", "65536"))
STRIDE = int(os.environ.get("DLP_STRIDE", "128"))

ner = pipeline(
    "token-classification",
    model=MODEL,
    aggregation_strategy="simple",
    stride=STRIDE,
)
```

then in `scan()`:

```python
    text = req.text
    truncated = len(text) > MAX_CHARS
    if truncated:
        text = text[:MAX_CHARS]
    findings = [...]  # same loop, over ner(text)
    return {"findings": findings, "truncated": truncated}
```

CRITICAL verification step: the transformers `TokenClassificationPipeline`
chunking API differs across versions (constructor `stride=` with
`aggregation_strategy` vs call-time kwarg; requires a fast tokenizer). Build
the image locally and TEST inside it before committing:

```bash
docker compose -f deploy/docker-compose.yml build dlp-bert
docker compose -f deploy/docker-compose.yml up -d dlp-bert
# entity at the TAIL of a large text MUST be found now:
python3 - <<'PY'
import json, urllib.request
big = ("x = compute(value)\n" * 1500)[:28000] + " My name is John Smith."
r = urllib.request.urlopen(urllib.request.Request(
    "http://127.0.0.1:8000/scan", json.dumps({"text": big}).encode(),
    {"Content-Type": "application/json"}))
out = json.load(r)
assert any(f["label"] == "PER" for f in out["findings"]), out
print("tail entity FOUND, truncated =", out.get("truncated"), ", findings:", len(out["findings"]))
PY
```

If the constructor kwarg is rejected, move `stride=STRIDE` to the call site
(`ner(text, stride=STRIDE)`); if both fail, implement manual windowing
(tokenizer with `return_overflowing_tokens=True, stride=STRIDE`, per-window
inference, offset-shift the spans, dedupe overlaps) — but try the pipeline
API first, it exists for exactly this.

Also verify the cap: text of MAX_CHARS+1000 → `"truncated": true`.

- [ ] **Step 2: Timing sanity**

Record scan time for the 28 KB payload in the report (expect low seconds on
dev CPU — full coverage costs more than the old silent truncation; that is
the point, and the gateway's scope/budget bound the impact).

- [ ] **Step 3: Commit**

```bash
git add deploy/dlp-bert/
git commit -m "feat(dlp-bert): sliding-window chunked scanning with DLP_MAX_CHARS cap"
```

---

### Task 4: Live verification (controller)

- [ ] Rebuild app + dlp-bert on the dev stack; enable model scanning (dev DLP config), scope default.
- [ ] Tail-entity repro through the FULL path (gateway alias with BERT on): plant a PER entity at the end of a large last-user message → incident/flag recorded (layer-2 finding present).
- [ ] Scope check with the sidecar log oracle: request with 6-message history → exactly 1 `/scan` (the last user message); flip scope to `all` in the console → same request → 4+ scans; flip back.
- [ ] Budget check: temporarily set budget to 1 ms via the console → chat still 200 (fail-open); restore 2000.
- [ ] Latency: time a chat with a ~30-message history, scope last_user — the DLP component must add well under 1 s (compare `airllm_component_duration_seconds` for dlp before/after if convenient).
- [ ] Full e2e regression + playwright DLP tab round-trip of the two new controls.
