# Capture store + DLP flywheel + audit — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Capture data-plane traffic (async, off the hot path) to an encrypted blob store + Postgres index, expose it for audit, and feed a labelling/oracle/retraining flywheel that improves the DLP model and catches its own false negatives.

**Architecture:** A non-blocking capture pipeline enqueues each finished request to a worker pool that writes the (redacted-by-default) body to a `blob.Store` and a metadata row to `capture_index`. An `airllm_auditor` role reads transcripts; a review queue collects gold labels; an async oracle re-scans captures with a stronger detector to surface false negatives; a dataset exporter emits training data.

**Tech Stack:** Go 1.26 (stdlib net/http mux), jackc/pgx/v5, redis/go-redis/v9, internal/secrets (AES-GCM), object storage (fs for dev, MinIO/GCS for deploy), existing internal/dlp + providers.

## Global Constraints

- Go module `github.com/rromenskyi/ipsupport-airllm`; Go 1.26; stdlib routed mux (no router dep).
- English-only repo (code/comments/commits/UI). No secrets in git. Apache-2.0.
- **Never block the request hot path** on capture I/O (enqueue + drop-with-counter when full).
- **Capture off by default**; **redacted-by-default**; bodies only in the blob store, never in Postgres.
- Encrypt blob bodies at rest (internal/secrets AES-GCM). Access gated by `airllm_auditor` (admin inherits); every transcript read is itself audit-logged.
- Loopback-only for local services (host has a public IP). Commit trailer: `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`.
- Run `go build ./... && go vet ./... && gofmt -l . && go test ./...` green before each commit.

## File Structure

- `internal/blob/blob.go` — `Store` interface (`Put/Get/Delete`).
- `internal/blob/fs.go` — filesystem impl (dev); `internal/blob/minio.go` — MinIO/S3 impl (deploy).
- `internal/capture/capture.go` — `Record`, `Pipeline` (channel + workers), sampler, retention sweeper.
- `internal/capture/store.go` — `capture_index` reads/writes (pgx).
- `internal/oracle/oracle.go` — async re-scan job + diff.
- `internal/dataset/dataset.go` — labelled export.
- `migrations/0004_capture.sql` — `capture_index`; `migrations/0005_auditor.sql` if role seed needed.
- `internal/httpapi/api_audit.go` — auditor + admin capture endpoints.
- `web/static/app.js` — Audit + Review console tabs, capture config.
- Wiring: `internal/httpapi/server.go`, `internal/httpapi/dlp.go`/`dataplane.go`/`messages.go`, `cmd/ipsupport-airllm/main.go`, `deploy/docker-compose.yml`.

---

## Phase 1 — Capture core

Outcome: finished requests are captured (sampled + all DLP incidents) to an encrypted blob + `capture_index`, off the hot path, with retention. No UI yet.

### Task 1.1: blob.Store interface + fs impl

**Files:**
- Create: `internal/blob/blob.go`, `internal/blob/fs.go`
- Test: `internal/blob/fs_test.go`

**Interfaces:**
- Produces: `type Store interface { Put(ctx, key string, data []byte) error; Get(ctx, key string) ([]byte, error); Delete(ctx, key string) error }`; `func NewFS(root string) (*FS, error)`.

- [ ] **Step 1: Write the failing test**
```go
package blob

import (
	"context"
	"testing"
)

func TestFSRoundTrip(t *testing.T) {
	s, err := NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(context.Background(), "a/b.bin", []byte("hello")); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(context.Background(), "a/b.bin")
	if err != nil || string(got) != "hello" {
		t.Fatalf("get=%q err=%v", got, err)
	}
	if err := s.Delete(context.Background(), "a/b.bin"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(context.Background(), "a/b.bin"); err == nil {
		t.Fatal("expected error after delete")
	}
}
```
- [ ] **Step 2: Run `go test ./internal/blob/` — expect FAIL (no package).**
- [ ] **Step 3: Implement** `blob.go` (the interface) and `fs.go`: `NewFS` stores under `root`; `Put` does `os.MkdirAll(filepath.Dir)` then atomic write (temp file + `os.Rename`); keys are sanitized (reject `..`); `Get`/`Delete` use `os.ReadFile`/`os.Remove`.
- [ ] **Step 4: Run `go test ./internal/blob/` — expect PASS.**
- [ ] **Step 5: Commit** `feat(blob): Store interface + filesystem impl`.

### Task 1.2: capture_index migration

**Files:** Create: `migrations/0004_capture.sql`

- [ ] **Step 1:** Write the migration: table `capture_index` with columns from the spec data model (`id uuid pk default gen_random_uuid()`, `ts timestamptz default now()`, `key_id uuid null`, `user_id uuid null`, `ingress_protocol`, `alias`, `provider_name`, `upstream_model`, `status int`, `prompt_tokens bigint`, `completion_tokens bigint`, `cost_usd numeric(12,6)`, `blob_key text`, `redacted bool`, `model_version text`, `detected jsonb default '{}'`, `review_status text default 'unreviewed'`, `oracle_status text default 'pending'`, `oracle_labels jsonb default '{}'`), indexes on `(ts desc)`, `(review_status)`, `(oracle_status)`. Add `model_version text default ''` to `dlp_incidents`.
- [ ] **Step 2:** Run the app (or `make compose-up`) against a fresh DB; confirm migration applies (query `information_schema.columns`).
- [ ] **Step 3: Commit** `feat(db): capture_index + dlp_incidents.model_version (migration 0004)`.

### Task 1.3: capture config (settings)

**Files:**
- Modify: `internal/httpapi/dlp.go` (or new `internal/httpapi/capture.go`) — add `captureConfig` + load/atomic, default disabled.
- Test: `internal/httpapi/capture_config_test.go`

**Interfaces:**
- Produces: `type captureConfig struct { Enabled bool; SampleRate float64; Redact bool; RetentionDays int; RawTraining bool; RawTTLHours int }`; `defaultCaptureConfig()` (Enabled=false, SampleRate=0, Redact=true, RetentionDays=30); `(s *Server) captureCfg() captureConfig`; `loadCapture(ctx)` from settings key `capture`.

- [ ] **Step 1: Write the failing test** asserting `defaultCaptureConfig()` is disabled + redact=true, and that an invalid `SampleRate` clamps to [0,1].
- [ ] **Step 2:** Run — FAIL.
- [ ] **Step 3:** Implement the struct, default, clamp, atomic load (mirror `dlpConfig`/`loadDLP`). Call `s.loadCapture(context.Background())` in `NewServer`.
- [ ] **Step 4:** Run — PASS.
- [ ] **Step 5: Commit** `feat(capture): config (off by default, redacted, retention)`.

### Task 1.4: capture pipeline (async, sampling, drop-with-counter)

**Files:**
- Create: `internal/capture/capture.go`, `internal/capture/store.go`
- Test: `internal/capture/capture_test.go`

**Interfaces:**
- Consumes: `blob.Store`, a `*pgxpool.Pool` (via a small `Inserter` interface for testability), `*secrets.Sealer`.
- Produces:
  - `type Record struct { KeyID, UserID, Ingress, Alias, Provider, UpstreamModel string; Status, PromptTokens, CompletionTokens int; CostUSD float64; ModelVersion string; Detected []dlp.Finding; Body []byte; HadIncident bool }`
  - `type Pipeline struct{...}`; `func NewPipeline(blob blob.Store, idx Inserter, sealer *secrets.Sealer, cfg func() Config) *Pipeline` where `Config{Enabled bool; SampleRate float64; Redact bool}`.
  - `func (p *Pipeline) Start(workers int)`, `func (p *Pipeline) Stop()`, `func (p *Pipeline) Enqueue(r Record)` (non-blocking; increments a dropped counter when the buffer is full), `func (p *Pipeline) Dropped() int64`.
  - Sampling: capture when `cfg().Enabled && (r.HadIncident || rand < SampleRate)`.

- [ ] **Step 1: Write the failing test** — a fake `Inserter` (records rows) + an in-memory `blob.Store`; enable capture (SampleRate=1); `Enqueue` a record; `Stop()` (drains); assert one blob written + one index row, and that `Body` was sealed (ciphertext != plaintext). Add a test that with `Enabled=false` nothing is written, and a test that an incident record is captured even at SampleRate=0.
```go
func TestPipelineCaptures(t *testing.T) {
	bs := newMemBlob()
	idx := &fakeInserter{}
	p := NewPipeline(bs, idx, testSealer(t), func() Config { return Config{Enabled: true, SampleRate: 1, Redact: true} })
	p.Start(2)
	p.Enqueue(Record{Ingress: "openai", Body: []byte("hi"), Status: 200})
	p.Stop()
	if len(idx.rows) != 1 || len(bs.objs) != 1 {
		t.Fatalf("rows=%d objs=%d", len(idx.rows), len(bs.objs))
	}
}
```
- [ ] **Step 2:** Run `go test ./internal/capture/` — FAIL.
- [ ] **Step 3:** Implement: buffered channel (size e.g. 1024), worker goroutines; each worker seals `Body` (or redacts first per `cfg().Redact` using `dlp.Redact` on the already-known `Detected` spans — NOTE: redaction is applied by the caller before Enqueue in the handler; here `Redact` only gates whether raw is allowed), writes blob `captures/<id>`, inserts index row via `Inserter`. `Enqueue` uses `select { case ch<-r: default: atomic dropped++ }`. `Stop` closes channel + waits.
- [ ] **Step 4:** Run — PASS.
- [ ] **Step 5: Commit** `feat(capture): non-blocking pipeline (seal + blob + index)`.

### Task 1.5: index writer (real pgx Inserter) + reader

**Files:** Modify: `internal/capture/store.go`; Test: integration via the running DB (manual) — unit-test the SQL builder with a fake.

**Interfaces:**
- Produces: `type PGInserter struct { PG *pgxpool.Pool }` implementing `Insert(ctx, IndexRow) error`; `IndexRow` mirrors `capture_index`; plus `List(ctx, filter) ([]IndexRow, error)` and `Get(ctx, id) (IndexRow, error)` for later phases.

- [ ] **Step 1:** Write a test that `PGInserter.Insert` builds the expected arg list (use a recording fake of the `Exec`-er interface).
- [ ] **Step 2:** Run — FAIL.
- [ ] **Step 3:** Implement `Insert` (INSERT into capture_index), `List` (filter by ts range / review_status / oracle_status, LIMIT), `Get`.
- [ ] **Step 4:** Run — PASS.
- [ ] **Step 5: Commit** `feat(capture): postgres index writer + reader`.

### Task 1.6: wire pipeline into the gateway

**Files:** Modify: `internal/httpapi/server.go` (hold `*capture.Pipeline`), `internal/httpapi/dlp.go` (return findings from `dlpEnforce` so the handler can capture them), `internal/httpapi/dataplane.go` + `messages.go` (enqueue after finalizeUsage), `cmd/ipsupport-airllm/main.go` (build blob.Store from config, start/stop pipeline).

**Interfaces:**
- Consumes: `Pipeline.Enqueue`, `captureCfg()`, the request/response content.
- Capture point: in the non-stream handlers, after `finalizeUsage`, build a `capture.Record` (body = JSON of the request messages + the response content; `Redact` applied if `cfg.Redact`; `HadIncident` from the DLP result; `Detected` spans; `ModelVersion` from dlp model). For streaming, capture the assembled text + usage at the end.

- [ ] **Step 1:** Add `CAPTURE_BLOB_DIR` (dev fs) / object-store env to config; in main build `blob.Store` (fs for dev) and `capture.NewPipeline(...)`, `Start(4)`, `defer Stop()`; pass into `httpapi.Deps`.
- [ ] **Step 2:** Modify `dlpEnforce` to also return `[]dlp.Finding` + `redactedBody string` so the handler can record exactly what was detected/sent.
- [ ] **Step 3:** In `handleChatCompletions` (non-stream) enqueue a `Record` after the response is marshalled; same in `handleMessages`; streaming handlers enqueue after the stream completes (assembled content).
- [ ] **Step 4:** Manual e2e: enable capture via a temporary settings row (`{"enabled":true,"sample_rate":1,"redact":true,"retention_days":30}`), send a chat, confirm a `capture_index` row + a blob file appear; send one with a secret, confirm `redacted=true` and the blob body is masked.
- [ ] **Step 5: Commit** `feat(capture): record requests/responses from both ingresses`.

### Task 1.7: retention sweeper

**Files:** Modify: `internal/capture/capture.go` (sweeper goroutine); Test: `internal/capture/capture_test.go`

**Interfaces:**
- Produces: `func (p *Pipeline) sweep(ctx, now time.Time)` — deletes index rows past `RetentionDays` (and raw past `RawTTLHours`) and their blobs.

- [ ] **Step 1:** Test (fake inserter+blob with an old row) that `sweep` deletes expired rows + blobs and keeps fresh ones (inject `now`).
- [ ] **Step 2:** Run — FAIL.
- [ ] **Step 3:** Implement `sweep` + a ticker (hourly) started in `Start`; needs a `List`+`Delete` path on the inserter/blob.
- [ ] **Step 4:** Run — PASS.
- [ ] **Step 5: Commit** `feat(capture): retention sweeper`.

---

## Phase 2 — Audit (auditor role + transcript views)

Outcome: an `airllm_auditor` can search and read transcripts in the console; every read is audit-logged.

### Task 2.1: auditor role + RBAC
**Files:** Modify: `internal/auth/auth.go` (add `AuditorRole = "airllm_auditor"`, `IsAuditor()` true for auditor OR admin), `internal/httpapi/session.go` (`requireAuditor` wrapper), `internal/seed/seed.go` (mock `auditor` user with random password, like operator).
- [ ] Test: `auth` unit test — admin and auditor pass `IsAuditor`, plain user fails. Steps: test→fail→impl→pass→commit `feat(auth): auditor role`.

### Task 2.2: audit search + view endpoints
**Files:** Create: `internal/httpapi/api_audit.go`; Modify: `server.go` routes; `internal/capture/store.go` (List/Get already from 1.5).
**Interfaces:** `GET /api/audit/captures?from&to&review_status&limit` → index rows (no body); `GET /api/audit/captures/{id}` → row + decrypted body; each behind `requireAuditor`; the `{id}` read calls `s.audit(actor,"audit.view",id,nil)`.
- [ ] Steps (TDD with httptest + a fake store): test list returns rows; test get decrypts body and writes an audit row; test non-auditor → 403. Commit `feat(audit): transcript search + view (access-logged)`.

### Task 2.3: console Audit tab
**Files:** Modify: `web/static/app.js` (add `audit` admin/auditor tab: filters + table + a row → transcript drawer).
- [ ] Steps: render list from `/api/audit/captures`; click row → fetch `/api/audit/captures/{id}` → show transcript; nav visible to auditor/admin. Commit `feat(web): audit console tab`. Verify via Playwright (extend `e2e/`).

---

## Phase 3 — Review + labelling

Outcome: reviewers triage captures into gold labels (confirmed / false_positive / false_negative with edited spans).

### Task 3.1: review endpoints
**Files:** Modify: `internal/httpapi/api_audit.go`, `internal/capture/store.go`.
**Interfaces:** `GET /api/audit/review` → captures where `review_status='unreviewed' OR oracle_status='suspect'`; `POST /api/audit/captures/{id}/review` body `{review_status, labels:[{label,start,end}]}` → updates row (`requireAuditor`, audited).
- [ ] Steps (TDD): test review update persists status+labels; test invalid status → 400. Commit `feat(review): review queue + labelling endpoint`.

### Task 3.2: review UI (span editor)
**Files:** Modify: `web/static/app.js` (Review tab: queue table + a transcript viewer with the detected spans highlighted; buttons confirm / mark FP / add a missed span (select text → label) → POST review).
- [ ] Steps: render queue; span add via a simple offset form (start/end/label) to keep it minimal; submit. Commit `feat(web): review/labelling console`.

---

## Phase 4 — Oracle (false-negative hunting, off hot path)

Outcome: a background job re-scans recent captures with a stronger detector and flags misses.

### Task 4.1: oracle engine interface + LLM impl
**Files:** Create: `internal/oracle/oracle.go`; Test: `internal/oracle/oracle_test.go`.
**Interfaces:** `type Engine interface { Scan(ctx, text string) ([]dlp.Finding, error) }`; an LLM impl that calls a provider (via the registry / a configured model) with a strict JSON-returning prompt and parses spans; config in `capture`/`dlp` settings (`oracle_enabled`, `oracle_model`).
- [ ] Steps (TDD with a fake LLM returning canned JSON): test Scan parses spans + filters; test malformed output → no findings, no crash. Commit `feat(oracle): engine interface + LLM-backed scanner`.

### Task 4.2: oracle job + false-negative diff + alert
**Files:** Create job in `internal/oracle/oracle.go` (ticker); Modify: `capture/store.go` (fetch `oracle_status='pending'`, update), reuse `internal/webhook` for `dlp.false_negative`.
**Interfaces:** `func (j *Job) runOnce(ctx)` — pulls pending captures, decrypts body, `Engine.Scan`, diffs vs `detected`; extra findings → `oracle_status='suspect'`, `oracle_labels=extra`, fire `dlp.false_negative` webhook + (if configured) `dlp_incidents` row; else `oracle_status='clean'`.
- [ ] Steps (TDD): seed a capture whose body has a name not in `detected`; runOnce → status suspect + labels set + webhook payload built. Commit `feat(oracle): false-negative detection + alert`.

---

## Phase 5 — Dataset export

Outcome: a labelled dataset artifact for offline fine-tuning + a documented runbook.

### Task 5.1: dataset export endpoint
**Files:** Create: `internal/dataset/dataset.go`; Modify: `api_audit.go` (`POST /api/admin/dataset/export` admin-only).
**Interfaces:** `func Export(ctx, store, blob, sealer, filter) (artifactKey string, count int, err error)` — selects reviewed captures (confirmed + false_negative), emits JSONL `{text, spans:[{label,start,end}]}` (gold from review labels, falling back to detected), writes the artifact to the blob store, returns its key.
- [ ] Steps (TDD with fakes): test export emits one JSONL line per reviewed capture with merged gold spans; test it skips unreviewed. Commit `feat(dataset): labelled training export`.

### Task 5.2: fine-tune runbook
**Files:** Create: `deploy/dlp-bert/TRAINING.md`.
- [ ] Document: pull the exported JSONL, fine-tune the token-classification model on a GPU node (HF Trainer skeleton command), push the new model, rebuild the sidecar with the new `DLP_MODEL` (or mount weights), confirm `model_version` flows into new captures/incidents to measure FN-rate improvement. Commit `docs(dlp-bert): fine-tune runbook`.

---

## Self-Review

- **Spec coverage:** capture (P1), audit (P2), review/labelling (P3), oracle/false-negatives (P4), dataset/flywheel (P5), redacted-by-default + encryption + off-hot-path + auditor RBAC + retention — all mapped. ✓
- **Type consistency:** `blob.Store`, `capture.Record`/`IndexRow`/`Pipeline`, `oracle.Engine`, `dlp.Finding` reused across tasks consistently. `captureConfig` mirrors `dlpConfig` pattern. ✓
- **Decisions (from spec, confirmed):** redacted-by-default; fs blob for dev + MinIO/GCS deploy; oracle pluggable starting LLM-via-provider; sampling 100% incidents + configurable % clean (default 0). ✓
