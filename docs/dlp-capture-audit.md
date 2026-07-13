# DLP, capture & audit

The gateway can detect secrets and PII in agent traffic, act on them, record
traffic for audit, and use reviewer-corrected captures to improve the detector
over time. Every part is **off by default** and configured at runtime
(**Admin → DLP**, or the admin API).

```
request ──▶ DLP scan ──▶ enforce (off|flag|redact|block) ──▶ provider
                │                                              │
                ▼                                              ▼
          dlp_incidents                                  capture pipeline
          + alert webhooks                          (async, off the hot path)
                                                           │
                                            ┌──────────────┼───────────────┐
                                            ▼              ▼                ▼
                                      capture_index   blob store      raw window
                                      (metadata)      (sealed body)   (sealed, TTL)
                                            │
                                    second-pass (flywheel)
                                  confirm / clear FP · find FN
                                            │
                                      review queue ──▶ gold labels ──▶ dataset export (JSONL)
```

## DLP detection

Two layers behind one `Scan` API:

1. **Deterministic (in-process).** Regular expressions for common credential
   shapes (cloud keys, provider keys, GitHub/Slack/Stripe tokens, JWTs, PEM/SSH
   private keys, TLS material) plus a Shannon-entropy check for high-entropy
   blobs. A mixed-character-class guard avoids flagging SHAs/UUIDs, and named
   rules win over the entropy heuristic. The detector returns only labelled byte
   spans — never the secret value — so callers can redact or alert without
   logging the secret.
2. **Contextual PII (BERT-NER sidecar, opt-in).** A FastAPI + transformers
   service (`dslim/bert-base-NER`) returns person/org/location spans the
   deterministic layer can't catch. It runs as the `dlp-bert` compose service
   under the `bert` profile; enable it and set its URL under **Admin → DLP**.
   See [`deploy/dlp-bert/README.md`](../deploy/dlp-bert/README.md). Sidecar
   failures degrade gracefully to the deterministic layer. The gateway
   load-balances requests across a pool of sidecar endpoints and fails open
   (skips the model scan) when all endpoints are busy, applying deterministic
   redaction only.

   By default (`model_scan_scope: last_user`) the sidecar only scans the
   newest user message: since the full conversation history is resent every
   turn, scanning it repeatedly would burn sidecar capacity on text already
   checked in an earlier turn, and the deterministic layer still covers every
   message in the request regardless of scope. Set `model_scan_scope: all` to
   scan every message each turn instead. `model_scan_budget_ms` (default
   `2000`) caps the total time the model layer may spend per request; once the
   budget is exhausted, remaining scans fail open and are skipped (metric
   reason `budget`). The sidecar itself scans long messages in chunks bounded
   by `DLP_MAX_CHARS`, so a single oversized message can't monopolize the
   budget or the sidecar's request.

### Enforcement actions

`action` is one of:

| Action | Effect |
|--------|--------|
| `off` | No scanning |
| `flag` | Scan and record an incident, but pass the request through unchanged |
| `redact` | Replace each detected span with `[REDACTED:<label>]` before dispatch |
| `block` | Reject the request with a client error listing the labels |

Detections are recorded to `dlp_incidents` (with secret-free samples) and, if
configured, delivered to **alert webhooks**. Each capture/incident is stamped
with the detector version (`model_version`, currently `regex+entropy/v1`) for
provenance.

### Prompts-only

DLP scans **prompts only** — the data an agent sends upstream. Model responses
are never scanned, redacted, or blocked. This is a deliberate design choice; the
`scan_responses` config field is reserved and inert, not a missing feature.

## Sensitive Info Detection

Beyond secrets, the detector ships a catalog of toggleable patterns (OpenRouter-
style guardrails). The operator chooses which run per workspace under **Admin →
DLP → Sensitive Info Detection**; `GET /api/admin/dlp/patterns` lists the catalog.

| Pattern | Category | Default | Notes |
|---------|----------|---------|-------|
| secret rules (`openai_key`, `jwt`, `private_key`, …) | secret | on | the credential detectors |
| `high_entropy` | secret | on | entropy heuristic (toggleable) |
| `email` | pii | off | email address |
| `phone` | pii | off | E.164 / common US/intl forms |
| `ssn` | pii | off | US `NNN-NN-NNNN` |
| `credit_card` | pii | off | digit run, **Luhn-validated** |
| `ip_address` | pii | off | IPv4 with octet-range validation |
| `person_name` | pii (model) | off | BERT `pii:PER` — *adds latency* |
| `address` | pii (model) | off | BERT `pii:LOC` — *adds latency* |
| `organization` | pii (model) | off | BERT `pii:ORG` — *adds latency* |

- **Toggles** are stored in `dlp.patterns` (label → on/off). A label absent from
  the map uses its default, so partial/legacy configs keep working; the model
  toggles only take effect when the BERT sidecar is enabled.
- **Custom patterns** (`dlp.custom_patterns`: `{label, regex, enabled}`) let the
  operator add their own. They are validated on save (must compile, ≤ 512 chars,
  ≤ 50 entries). Detection uses Go's RE2 engine, which is linear-time — operator
  regexes cannot cause catastrophic backtracking (no ReDoS).

A toggled-on PII or custom pattern flows through the same `action`
(`flag`/`redact`/`block`), incidents, and webhooks as the secret detectors.

## Alert webhooks

Endpoints registered under **Admin → DLP** receive HMAC-signed POSTs
(`X-AirLLM-Signature`) for DLP events. Beyond `dlp.incident`, the flywheel emits
`dlp.false_negative` (a secret the fast layer missed) and `dlp.alert_cleared`
(a fast-layer alert the stronger pass could not confirm). Payloads never contain
the secret value.

## Capture store

When capture is enabled, the gateway records request/response traffic
**asynchronously, off the hot path** — it never blocks or slows the request.

- **Redacted by default.** Stored bodies have secrets masked regardless of the
  DLP action; raw secrets do not reach the durable store unless you explicitly
  opt into the raw window (below).
- **Sealed at rest.** Bodies live in the blob store, AES-256-GCM sealed; only
  metadata and DLP weak-labels live in `capture_index` (Postgres).
- **Sampling.** `sample_rate` controls the fraction of ordinary traffic
  captured; **incidents are always captured**.
- **Retention.** A sweeper deletes rows and blobs older than `retention_days`.
- **Backpressure-safe.** The pipeline uses a bounded buffer; under overload it
  drops records (with a counter) rather than blocking the request path.

## The flywheel

The capture store feeds a loop that improves the detector.

### Second-pass

An off-hot-path job (**Admin → DLP → second-pass**, off by default) re-scans
pending captures with a stronger engine. For each capture it:

1. **Confirms or clears** the fast-layer detections (clearing a false positive).
2. **Hunts misses** the fast layer didn't catch (false negatives).
3. Emits `dlp.false_negative` / `dlp.alert_cleared` webhooks and surfaces the
   capture in the review queue.

### The raw training window

Accurate confirm/clear and training-data export need text whose byte offsets
line up with the detections. On a redacted stream they don't. The **raw window**
(`raw_training`, default off) stores a second, **un-redacted** copy of the body,
sealed, with a short TTL (`raw_ttl_hours`). Second-pass and dataset export
prefer it while it is unexpired; both fall back to the redacted body afterward.

Safety properties:

- The raw copy is the only place a real secret is stored, it is encrypted, and
  it is short-lived.
- `raw_ttl_hours` is clamped to `≤ retention_days × 24`, and the retention sweep
  deletes the raw copy along with its row — a raw copy can never outlive its row
  or be orphaned.
- Because DLP `redact` masks the request in place, the pipeline preserves the
  pre-redaction originals specifically to build this un-redacted copy; the
  durable body stays redacted.

### Review & dataset export

Auditors label captures in the **Review** queue (`review_status` ∈ `confirmed`,
`false_positive`, `false_negative`, `unreviewed`) and may attach corrected
**gold labels**. `POST /api/admin/dataset/export` then emits a JSONL artifact of
reviewed captures — one line per message with attributed spans,
`{"text": "...", "spans": [{"label","start","end"}]}` — for offline fine-tuning.
The fine-tune runbook is [`deploy/dlp-bert/TRAINING.md`](../deploy/dlp-bert/TRAINING.md).

## Audit

The `airllm_auditor` role grants read access to captured transcripts without any
admin powers. Auditors can search captures (metadata + DLP labels) and open a
single capture's decrypted body — and **every body view is itself recorded in
the audit log**. The admin audit log (`/api/admin/audit`) records control-plane
mutations (DLP/capture/role/provider edits, reviews, exports, webhook changes)
with actor, action, and target.

## Known limitations

- **Second-pass span coordinates.** The second-pass diffs per-message detection
  offsets against engine offsets taken over the whole stored-body JSON wrapper;
  the coordinate systems can disagree. Dataset export handles this correctly
  (per message). Treat second-pass confirm/clear as advisory until this is
  unified.
- **The `redacted` flag** on a capture row reflects the capture *config* at
  enqueue time, not whether masking actually occurred for that specific row.

See [Configuration](configuration.md) for every field and default.
