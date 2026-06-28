# Capture store + DLP flywheel + audit — design

A subsystem for AirLLM that (1) captures data-plane traffic to durable
storage, (2) uses it as an audit trail, and (3) feeds a labelling/retraining
flywheel that makes the DLP BERT model better over time, including catching
its own misses (false negatives) via an off-hot-path oracle.

- **Status:** design, pre-implementation. Builds on the shipped gateway
  (usage_ledger, internal/dlp deterministic + BERT-sidecar layers,
  incidents, webhooks).
- **Repo:** `ipsupport-airllm`.

## Goals

- Optionally record full request/response transcripts for **audit**.
- Build a **training dataset** for the DLP model from real traffic, with
  weak labels (detector output) plus human/oracle gold labels.
- **Find false negatives**: surface sensitive content the fast detectors
  missed, alert on it, and turn it into high-value training examples.
- Be **safe by default**: the capture store must not become the very leak
  DLP prevents — encryption, access control, retention, redacted-by-default.

## Non-goals

- The training job itself (fine-tuning runs offline on a GPU node as a
  separate runner; this subsystem produces the dataset + consumes the
  resulting model, it does not train in-process).
- Real-time response streaming capture fidelity beyond best-effort (v1
  captures the assembled response).
- Multi-region / WORM compliance storage (note as future).

## Architecture

```
request ─▶ gateway (auth, policy, limits, DLP) ─▶ provider ─▶ response
                          │ (async, off hot path)
                          ▼
                  capture pipeline ──▶ object store (bodies, encrypted)
                          │                 +
                          └──────────▶ Postgres capture_index (metadata + DLP labels + ref)
                                            │
        ┌───────────────────────────────────┼───────────────────────────┐
        ▼                                   ▼                             ▼
  audit views (auditor role)     async second-pass job          labelling/review UI
  (search, view transcript)   (stronger model: 1. verify each   (confirm / mark FP / mark FN)
                               detection -> confirm OR clear            │
                               the alert; 2. find misses ->             ▼
                               false negatives -> alert)      dataset export ─▶ offline fine-tune
                                                                          ─▶ new model ─▶ sidecar swap
```

### Capture pipeline (off the hot path)
- After the response is produced, the handler enqueues a capture record on
  an in-process buffered channel; a worker pool writes it. The request path
  never blocks on capture I/O (drop-with-counter if the buffer is full, so
  capture can never degrade serving).
- **Sampling**: configurable rate (e.g. 0.0–1.0). Always-capture for
  requests with a DLP incident (those are the valuable ones).
- **Redaction mode (default ON)**: store the DLP-redacted content, not raw.
  Note: redaction masks only *detected* spans; *missed* secrets remain in
  the stored text (that is what the oracle hunts) — so the store is still
  sensitive and must be encrypted + access-controlled. A separate explicit
  "raw training window" mode (encrypted, short TTL, restricted) can capture
  unredacted content when actively building a dataset.

### Storage
- **Bodies** → object store (MinIO/GCS; pluggable `Blob` interface), one
  object per capture, **encrypted** (reuse `internal/secrets` AES-GCM or
  bucket SSE), keyed by capture id.
- **Index** → Postgres `capture_index`: metadata + DLP labels + blob ref;
  drives audit search and the review queue. Bodies are never put in PG.
- **Retention**: per-mode TTL (audit vs raw-training); a sweeper deletes
  expired objects + index rows.

## Data model (Postgres)

- `capture_index`
  - `id uuid`, `ts`, `key_id`, `user_id`, `ingress_protocol`, `alias`,
    `provider_name`, `upstream_model`, `status`, `prompt_tokens`,
    `completion_tokens`, `cost_usd`
  - `blob_key text` (object store ref), `redacted bool`, `model_version text`
  - `detected jsonb` (DLP findings at capture time: labels + spans)
  - `review_status text` (unreviewed | confirmed | false_positive | false_negative)
  - `oracle_status text` (pending | clean | suspect), `oracle_labels jsonb`
  - index on (ts desc), (review_status), (oracle_status)
- `capture_config` (settings row): `enabled`, `sample_rate`, `redact`,
  `retention_days`, `raw_training bool`, `raw_ttl_hours`, oracle config.
- Reuse existing `dlp_incidents`; add `model_version` to it for eval.

## Components (Go)

- `internal/capture` — pipeline (channel + workers), `Record` type, sampler,
  redaction hook, retention sweeper.
- `internal/blob` — `Store` interface (`Put/Get/Delete`), impls: `minio`/`gcs`
  (and a `fs` impl for local dev).
- `internal/oracle` — async job: pulls recent captures, runs a stronger
  detector (a larger model / an LLM via our own provider registry — Ollama
  fits here, OFF the hot path), diffs against `detected`; on extra findings
  sets `oracle_status=suspect` + `oracle_labels`, files an alert
  (`dlp.false_negative` webhook), and queues for review.
- `internal/dataset` — export labelled captures to a training format
  (JSONL token spans / CoNLL) for the offline fine-tune.
- httpapi: auditor + admin endpoints (below).

## API + UI

- **Auditor** (`airllm_auditor` role; admin inherits):
  - `GET /api/audit/captures?filters` — search index.
  - `GET /api/audit/captures/{id}` — metadata + decrypted body (access-logged).
  - `GET /api/audit/review` — queue (unreviewed + oracle-suspect).
  - `POST /api/audit/captures/{id}/review` — set review_status + gold spans.
- **Admin**:
  - `GET/PUT /api/admin/capture` — capture config.
  - `POST /api/admin/dataset/export` — produce a dataset artifact (counts +
    blob ref).
- **Console**: an "Audit" area (transcript search + viewer), a "Review"
  queue (confirm / mark FP / mark FN with span editing), and capture config
  under admin. Reuse existing dark UI + tables/modals.

## Flywheel loop

1. Traffic captured (sampled + all-incidents) with detector output as weak
   labels and `model_version`.
2. Oracle re-scans async → candidate false negatives → alert + review queue.
3. Auditor reviews: confirm / FP / FN, editing spans → gold labels.
4. `dataset export` emits gold + high-confidence weak labels.
5. Offline fine-tune (GPU node, separate runner) → new model + version.
6. Swap the sidecar image / `DLP_MODEL`; new `model_version` flows into
   captures/incidents so improvement is measurable (FN rate over time).

## Security / privacy / compliance

- Capture is **off by default**; enabling it is a deliberate, logged admin
  action (it records employee prompts — a surveillance/compliance matter).
- **Encrypted at rest**; bodies only in the object store, never in PG.
- **Redacted-by-default**; raw capture only in an explicit, time-boxed,
  restricted "training window".
- Access gated by `airllm_auditor`; every transcript view is itself
  audit-logged. Retention TTL enforced by the sweeper.
- The store may still contain *undetected* secrets (inherent to FN-hunting)
  — treat it as crown-jewels regardless of redaction mode.

## Phasing

1. **Capture core**: `blob` (fs+minio), `capture` pipeline (async, sampling,
   redact), `capture_index`, config, retention sweeper.
2. **Audit UI**: auditor role + search/view endpoints + console area
   (access-logged).
3. **Review + labelling**: review queue, review endpoint, span editor UI,
   `model_version` on incidents.
4. **Oracle**: async stronger-model re-scan → false-negative alerts + queue.
5. **Dataset export**: labelled export artifact; document the offline
   fine-tune runner + sidecar model swap.

## Open decisions (confirm before implementation)

- **Redacted-by-default**: store DLP-redacted content by default, raw only in
  an explicit training window? (recommended: yes)
- **Object store for dev**: ship an `fs` blob impl for the local mock and
  `minio`/`gcs` for deploy? (recommended: yes)
- **Oracle engine**: a bigger BERT, or an LLM (cluster Ollama) as the
  off-hot-path oracle, or both behind a config? (recommended: pluggable,
  start with LLM-via-provider since it needs no new model)
- **Default sampling**: capture 100% of incidents + N% of clean traffic?
  (recommended: 100% incidents, configurable % clean, default 0% clean until
  a training window is opened)
