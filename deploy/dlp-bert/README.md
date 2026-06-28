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

Defaults to `Isotonic/distilbert_finetuned_ai4privacy_v2` (ai4privacy PII NER);
override with the `DLP_MODEL` build arg / env var. CPU inference is ~10–50 ms
for short prompts.
