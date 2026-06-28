# ipsupport-airllm

Self-hosted, OIDC-governed LLM gateway. A single Go service exposes
OpenAI- and Anthropic-compatible endpoints to internal coding agents,
authenticates them by API key, enforces per-key model policy and rolling
usage limits, routes to upstream providers (OpenAI, OpenRouter, xAI,
Anthropic), and meters tokens/cost. A static SPA console, served by the same
binary, provides self-service key management and admin analytics.

> **Status: full local mock.** The whole pipeline (key-auth → policy →
> limits → routing/fallback → metering → ledger → console) runs locally
> against a **mock upstream provider** and **password login** (no OIDC). Real
> providers and generic OIDC are wired on the kubernetes deploy.

## Architecture

```
                 ipsupport-airllm (one Go binary)
  control-plane (session cookie)     data-plane (Bearer API key)
    /auth/login · /api/* · SPA "/"     /v1/chat/completions · /v1/models
                                       /v1/messages
                         │                       │
                  Postgres (durable)        Redis (rolling counters)
```

- **Dual ingress:** OpenAI (`/v1/chat/completions`, `/v1/models`) and
  Anthropic (`/v1/messages`), both non-streaming and SSE.
- **Routing:** model alias catalog with priority fallback, plus explicit
  `provider/model` passthrough when a key's policy allows it.
- **Policy:** per-role allowed-model lists and limits, snapshotted onto each
  API key at issue time.
- **Limits:** per-key rolling windows (5h / 24h / 7d) in tokens and USD,
  check-before / increment-after.
- **Console:** login, dashboard, self-service keys, usage, and an admin area
  (roles, aliases, providers, pricing, users, keys, usage, audit).

See [`docs/superpowers/specs`](docs/superpowers/specs/) for the design and
plan, and [`docs/translation.md`](docs/translation.md) for the protocol
translation approach.

## Quickstart (local mock)

```sh
make compose-up        # builds + runs postgres, redis, and the gateway
```

All ports bind to **127.0.0.1 only** (this is intentional — never expose an
unauthenticated service). Then:

1. Get the generated admin password from the logs:
   ```sh
   docker compose -f deploy/docker-compose.yml logs app | grep "mock login credential"
   ```
2. Open the console at <http://127.0.0.1:8080> and sign in as `admin`.
   (Reach a remote box via an SSH tunnel / `kubectl port-forward`.)
3. **API Keys → Create key**, copy the token (shown once).
4. Point any OpenAI-compatible client at the gateway:
   ```sh
   curl http://127.0.0.1:8080/v1/chat/completions \
     -H "Authorization: Bearer <your-token>" \
     -d '{"model":"mock-gpt","messages":[{"role":"user","content":"hello"}]}'
   ```
   Or the Anthropic shape at `/v1/messages` (`x-api-key: <token>`).
5. Watch **Usage** update in the console.

### Mock behaviours

- A fixed dev API key is seeded for scripting:
  `air_dev_demo00000000000000000000000000000z`.
- Seeded model aliases: `mock-gpt` (single target) and `mock-fallback`
  (failing primary → healthy secondary, to demo fallback).
- The mock returns normal content by default; include `tooltest` in the user
  message (with `tools` set) to make it emit a tool call.
- `stream: true` is supported on both ingresses.

## Development

```sh
make build     # build the binary
make test      # unit tests
make vet       # go vet
make run       # run against local DATABASE_URL / REDIS_URL

# run the binary against the compose datastores (loopback)
DATABASE_URL='postgres://airllm:airllm@127.0.0.1:55432/airllm?sslmode=disable' \
REDIS_URL='redis://127.0.0.1:56379/0' \
HTTP_ADDR='127.0.0.1:8099' ENV=dev AUTH_MODE=mock ./bin/ipsupport-airllm
```

## Configuration

| Env | Default | Notes |
|-----|---------|-------|
| `HTTP_ADDR` | `:8080` | listen address |
| `DATABASE_URL` | — (required) | Postgres DSN |
| `REDIS_URL` | `redis://localhost:6379/0` | Redis URL |
| `ENV` | `dev` | `dev` \| `prod`; API-key environment tag; dev seeds mock data |
| `AUTH_MODE` | `mock` | `mock` (password login) \| `oidc` (deploy) |

## License

Apache-2.0. See [LICENSE](LICENSE).
