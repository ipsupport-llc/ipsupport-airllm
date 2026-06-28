# ipsupport-airllm

Self-hosted, OIDC-governed LLM gateway. A single Go service exposes OpenAI- and
Anthropic-compatible endpoints to internal coding agents, authenticates them by
API key, enforces per-key model policy and rolling usage limits, routes to
upstream providers (OpenAI, OpenRouter, xAI, Anthropic) with balancing and
fallback, and meters tokens/cost. It can scan agent prompts for secrets and PII
(DLP), capture traffic for audit, and feed reviewer-corrected captures back into
the detector (the flywheel). A static SPA console, served by the same binary,
provides self-service key management and an admin area.

> **Status: full local mock.** The whole pipeline runs locally against a **mock
> upstream provider** and **password login** (no OIDC). Real providers and
> generic OIDC are wired on the kubernetes deploy.

## Architecture

```
                 ipsupport-airllm (one Go binary)
  control-plane (session cookie)     data-plane (Bearer / x-api-key)
    /auth/login · /api/* · SPA "/"     /v1/chat/completions · /v1/models
                                       /v1/messages
                         │                       │
                  Postgres (durable)        Redis (rolling counters)
                                            Blob store (sealed bodies)
```

- **Dual ingress** — OpenAI (`/v1/chat/completions`, `/v1/models`) and Anthropic
  (`/v1/messages`), both non-streaming and SSE.
- **Routing** — model alias catalog with priority fallback and per-provider
  concurrency/balancing; explicit `provider/model` passthrough when policy allows.
  All-busy returns `429`, never a crash.
- **Policy & limits** — per-role allowed models, snapshotted onto each key;
  per-key rolling windows (5h / 24h / 7d) in tokens and USD.
- **DLP** — deterministic (regex + entropy) plus an opt-in BERT-NER sidecar;
  `off`/`flag`/`redact`/`block`; HMAC alert webhooks. Prompts only, by design.
- **Capture, flywheel & audit** — async, off-hot-path, redacted-by-default
  capture; a second-pass that confirms/clears and finds misses; a review queue;
  JSONL dataset export; an `airllm_auditor` role with access-logged transcript
  views.
- **Console** — login, dashboard, self-service keys, usage, and an admin area
  (users, keys, roles, aliases, providers, pricing, DLP, capture, audit).

## Quickstart (local mock)

```sh
make compose-up        # builds + runs postgres, redis, and the gateway
```

All ports bind to **127.0.0.1 only** (intentional — never expose an
unauthenticated service or an auth-less datastore). Then:

```sh
# 1. get the generated admin password
docker compose -f deploy/docker-compose.yml logs app | grep "mock login credential"
# 2. open http://127.0.0.1:8080, sign in as admin, create an API key, then:
curl http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer <your-token>" \
  -d '{"model":"mock-gpt","messages":[{"role":"user","content":"hello"}]}'
```

Full walkthrough: [docs/getting-started.md](docs/getting-started.md).

## Documentation

| Guide | What it covers |
|-------|----------------|
| [Getting started](docs/getting-started.md) | Run the mock, first API call, console tour, BERT sidecar |
| [Configuration](docs/configuration.md) | Every env var and runtime setting |
| [API reference](docs/api.md) | Every endpoint — data-plane, control-plane, admin, audit |
| [Architecture](docs/architecture.md) | Components, request flow, storage, concurrency |
| [DLP, capture & audit](docs/dlp-capture-audit.md) | Scanning, the capture store, the flywheel, the audit trail |
| [Operations](docs/operations.md) | Building, testing, migrations, deploy, security posture |

## Development

```sh
make build      # build the binary
make test       # unit tests
make test-race  # unit tests under the race detector
make vet        # go vet
make run        # run against local DATABASE_URL / REDIS_URL
```

## Configuration (summary)

| Env | Default | Notes |
|-----|---------|-------|
| `DATABASE_URL` | — (required) | Postgres DSN |
| `HTTP_ADDR` | `:8080` | listen address |
| `REDIS_URL` | `redis://localhost:6379/0` | Redis URL |
| `ENV` | `dev` | `dev` \| `prod`; API-key tag; dev seeds mock data |
| `AUTH_MODE` | `mock` | `mock` (password login) \| `oidc` (deploy) |
| `AIRLLM_MASTER_KEY` | — | base64 32-byte AES key; **required in prod**, derived in dev |
| `CAPTURE_BLOB_DIR` | `capture-blobs` | writable dir for capture blobs |

See [docs/configuration.md](docs/configuration.md) for the full reference,
including the runtime (hot-reloaded) DLP/capture/second-pass settings.

## License

Apache-2.0. See [LICENSE](LICENSE).
