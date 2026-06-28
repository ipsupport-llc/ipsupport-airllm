# ipsupport-airouter

Self-hosted, OIDC-governed LLM gateway. A single Go service exposes
OpenAI- and Anthropic-compatible endpoints to internal coding agents,
authenticates them by API key, enforces per-key model policy and rolling
usage limits, routes to upstream providers (OpenAI, OpenRouter, xAI,
Anthropic), and meters tokens/cost. A SvelteKit SPA, served by the same
binary, provides self-service key management and admin analytics.

> Status: early development. The current build targets a **full local
> mock** — real pipeline (key-auth → policy → limits → routing →
> metering → ledger → SPA) against a mock upstream provider and dev auth
> (no OIDC). Real providers and OIDC are wired on the kubernetes deploy.

## Design

- [`docs/superpowers/specs/2026-06-27-ipsupport-airouter-design.md`](docs/superpowers/specs/2026-06-27-ipsupport-airouter-design.md)
- [`docs/superpowers/specs/2026-06-27-ipsupport-airouter-plan.md`](docs/superpowers/specs/2026-06-27-ipsupport-airouter-plan.md)

## Quickstart (local mock)

```sh
# bring up postgres + redis + app
make compose-up

# health
curl -s localhost:8080/healthz
curl -s localhost:8080/readyz
```

## Development

```sh
make tidy     # go mod tidy
make build    # build the binary
make vet      # go vet
make test     # run tests
make run      # run against a local DATABASE_URL / REDIS_URL
```

## License

Apache-2.0. See [LICENSE](LICENSE).
