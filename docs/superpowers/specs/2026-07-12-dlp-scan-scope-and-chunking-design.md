# DLP Model Scan: Honest Chunking + Scan Scope + Request Budget — Design

**Date:** 2026-07-12
**Status:** approved

## Problem (observed in production)

Two compounding defects make the layer-2 BERT scan unusable for long-context
clients (coding agents) and quietly weak for everyone:

1. **Silent truncation.** The sidecar runs `ner(text)` with no
   truncation/stride config: inputs beyond the model's 512-token window are
   silently cut — an entity at the tail of a 28 KB message is NOT found
   (verified live). Coverage claims are false for anything but short chat.
2. **Per-message sequential scans of the full history.** The gateway scans
   every message of every request (dlp.go message loop), each with its own 2 s
   timeout. Coding agents resend the whole conversation each turn: 50 messages
   × ~0.5 s (CPU pod) ≈ 25 s added per request, re-paid every turn; concurrent
   requests saturate the sidecar's CPU and everything stalls.

## Fix — three parts

### A. Sidecar: sliding-window chunking (no silent truncation)

`deploy/dlp-bert/app.py`:

- Call the pipeline with stride-based chunking so the full text is scanned in
  overlapping windows and entity offsets are aggregated:
  `ner(text, stride=DLP_STRIDE)` (fast tokenizer required — dslim/bert-base-NER
  has one). Implementer must verify the exact transformers API for
  token-classification chunking against the installed version at image build
  and adapt (constructor vs call kwarg).
- Hard cap: `DLP_MAX_CHARS` env (default `65536`). Longer inputs are scanned
  only up to the cap; the response gains `"truncated": true` so the gateway
  (future) and logs can tell. Response stays `{"findings": [...]}`-compatible.
- `/healthz` unchanged.

### B. Gateway: model-scan scope (default: last user message)

New DLP config field `model_scan_scope`: `"last_user"` (default) | `"all"`.

- `last_user`: the model scan runs ONLY on the last message with role `user`
  in the request. Rationale: clients resend history every turn; each user
  message is scanned the first time it appears (as the last user message of
  that turn). Assistant content is provider output — covered by the separate
  `scan_responses` option. Layer-1 deterministic scanning still runs on EVERY
  message (cheap).
- `all`: previous behavior (every message), for operators who want it.
- Stored configs predate the field: empty string normalizes to `last_user`
  (this intentionally changes the default — the old default is the defect).
- Console: a select in the DLP tab next to the other model settings.

### C. Gateway: per-request model-scan budget

New DLP config field `model_scan_budget_ms` (int, default `2000`, `0` →
default). One budget context is created per request before the message loop;
every model scan draws from it (replacing the per-message 2 s timeout). When
the budget is exhausted, remaining scans are skipped fail-open with a new
skip-metric reason `budget` (visible, not silent). With scope `last_user` the
budget effectively bounds the single scan; with `all` it bounds the whole
request.

## Config / API / UI surface

- `dlpConfig`: `ModelScanScope string`, `ModelScanBudgetMS int` (both
  round-trip through the existing `GET/PUT /api/admin/dlp` settings JSON;
  unknown-field-safe for old consoles).
- Console DLP tab: scope select (`last_user` / `all`) + budget number input.
- Docs: configuration.md DLP table rows; dlp-capture-audit.md updated to
  describe scope semantics and the budget.

## Testing

- Gateway unit (existing dlp harness + httptest sidecar): scope `last_user` →
  exactly one sidecar hit for a 3-message request (system/user/assistant tail
  ordering respected — last USER, not last message); scope `all` → one hit
  per non-empty message; budget: a slow fake sidecar + small budget → first
  scan runs, remaining skipped, no failure to the client.
- Sidecar (live, controller): rebuild image; the tail-entity repro MUST now
  find the entity at the end of a 28 KB text; timing recorded; `DLP_MAX_CHARS`
  cap respected with `truncated: true`.
- Regression: full e2e suite; per-alias toggle still gates everything.

## Out of scope

- No incremental cross-request dedup (stateless gateway).
- No GPU sidecar, no model swap.
- No change to layer-1, redaction, incidents, capture.
