# DLP BERT-NER sidecar

A small HTTP service that runs a transformers token-classification (NER) model
and returns labelled character spans. It is **layer 2** of the gateway's DLP:
the deterministic regex/entropy layer (in the Go binary) handles exact secrets
and tokens; this sidecar catches fuzzy/contextual PII (names, addresses, etc.).

## Contract

```
POST /scan   {"text": "..."}  ->  {"findings": [{"label","start","end","score"}]}
GET  /healthz
```

The gateway points at it via the DLP settings (`model_url`, e.g.
`http://dlp-bert:8000`, plus a `model_min_score`). It receives only prompt
text, never API keys, and stores nothing.

## Run

It is heavy (torch + model weights), so it runs under an opt-in compose
profile rather than by default:

```sh
docker compose -f deploy/docker-compose.yml --profile bert up -d --build dlp-bert
```

Then in the console: **Admin → dlp → Use model sidecar**, URL
`http://dlp-bert:8000`. On real infra (GPU node / cluster) run it there and
point `model_url` at its service.

## Model

Defaults to `dslim/bert-base-NER` (BERT-base fine-tuned on CoNLL-2003: PER,
ORG, LOC, MISC) — the actual default baked in by `Dockerfile`'s `DLP_MODEL`
build arg, and what every image released via `.github/workflows/release.yml`
ships with (that workflow passes no `DLP_MODEL` override). Override at build
time with `--build-arg DLP_MODEL=<hf-model-id>` together with
`--build-arg DLP_MODEL_REVISION=<commit sha or branch>` (the default revision
pins the default model's commit), or at runtime with the `DLP_MODEL` env var if
running from a local checkout instead of the prebuilt image. The image runs as
uid 10001 with `HF_HUB_OFFLINE=1`: it serves only the baked-in model and never
contacts the Hugging Face Hub. Python dependencies are pinned in
`requirements.txt`, the base image by digest. CPU inference is ~10–50 ms for short prompts.

Swapping in a model with a different label vocabulary (e.g. an ai4privacy
PII model, which emits labels like `EMAIL`/`GIVENNAME`/`STREET` instead of
`PER`/`ORG`/`LOC`) also needs `modelToggleLabel` in
`internal/httpapi/dlp.go` updated to recognize the new labels — otherwise
the per-entity enable/disable toggles in the admin console silently do
nothing for them (findings still come through, just always-on).
