# Getting started

This walks through running the gateway locally as a **full mock** — the entire
pipeline (key-auth → policy → limits → routing/fallback → metering → ledger →
console) against a mock upstream provider and password login (no OIDC, no real
provider credentials).

## Prerequisites

- Docker + Docker Compose (for the one-command path), **or**
- Go 1.26 and a reachable Postgres + Redis (for the from-source path).

## Run with Docker Compose

```sh
make compose-up        # builds + runs postgres, redis, and the gateway
```

This starts three services. Everything binds to **127.0.0.1 only** — see the
[security posture](operations.md#security-posture) for why that matters on a
host with a public IP. Schema migrations are applied automatically on boot.

| Service | Host port (loopback) | Notes |
|---------|----------------------|-------|
| gateway | `127.0.0.1:8080` | override with `APP_BIND` (see below) |
| postgres | `127.0.0.1:55432` | offset port; never exposed publicly |
| redis | `127.0.0.1:56379` | no auth; never exposed publicly |

To publish the gateway on a specific private interface instead of localhost,
copy [`deploy/.env.example`](../deploy/.env.example) to `deploy/.env` (git-ignored)
and set `APP_BIND`, e.g. `APP_BIND=10.0.0.2:8088`.

### Sign in

On first boot the gateway creates a persistent bootstrap admin. If
`AIRLLM_ADMIN_PASSWORD` is not set, a random password is generated and
**logged once** at `WARN`:

```sh
docker compose -f deploy/docker-compose.yml logs app | grep "bootstrap admin"
```

The password is stored permanently — it does **not** change on restart. Set
`AIRLLM_ADMIN_PASSWORD` in `deploy/.env` to control it from the start.

Open the console at <http://127.0.0.1:8080> and sign in as `admin` (or the
name set by `AIRLLM_ADMIN_USERNAME`). (Reach a remote box via an SSH tunnel or
`kubectl port-forward` — do not publish the port on a public NIC.)

In `dev` mode, sample `operator` (role `airllm_user`) and `auditor` (role
`airllm_auditor`) accounts are seeded with the fixed dev-only password
`devpass123` (a non-secret constant, never used outside local development).
From the console an admin can create additional users under **Admin → Users**.

### Enabling OIDC

To switch to OIDC instead of password login, set `AUTH_MODE=oidc` and supply
the `OIDC_*` variables in `deploy/.env` (see
[Configuration](configuration.md#oidc-settings-required-when-auth_modeoidc)).
The login screen will show a "Sign in with SSO" button rather than a password
form.

## Make a first API call

1. In the console: **API Keys → Create key**, then copy the token (shown once).
2. Point any OpenAI-compatible client at the gateway:

   ```sh
   curl http://127.0.0.1:8080/v1/chat/completions \
     -H "Authorization: Bearer <your-token>" \
     -d '{"model":"mock-gpt","messages":[{"role":"user","content":"hello"}]}'
   ```

3. Or use the Anthropic shape at `/v1/messages` with `x-api-key: <token>`.
4. Watch **Usage** update in the console.

A fixed dev key is also seeded for scripting (when `ENV=dev` and
`AUTH_MODE=local`, including the `mock` alias): `air_dev_demo00000000000000000000000000000z`.

## Console tour

| Page | Audience | Purpose |
|------|----------|---------|
| Dashboard | any user | usage cards + copy-paste connection details |
| API Keys | any user | self-service key create/revoke |
| Usage | any user | rolling-window token/cost totals |
| Captures | auditor | search captured traffic (metadata + DLP labels) |
| Review | auditor | label captures and inspect second-pass results |
| Admin console | admin | users, keys, roles, aliases, providers, pricing, DLP, audit |

## Mock behaviours

- **Aliases:** `mock-gpt` (single target) and `mock-fallback` (failing primary →
  healthy secondary, to demonstrate fallback).
- **Tool calls:** include `tooltest` in the user message (with `tools` set) to
  make the mock emit a tool call.
- **Fallback/saturation:** the mock provider exposes a `fail` model (returns a
  retryable error) and a `slow` model (sleeps ~300 ms) for exercising fallback
  and concurrency limits.
- **Streaming:** `stream: true` works on both ingresses.

## Optional: the BERT DLP sidecar

The deterministic DLP layer runs in-process. A second, contextual PII layer
(BERT-NER) runs as an opt-in sidecar:

```sh
docker compose -f deploy/docker-compose.yml --profile bert up --build dlp-bert
```

Then in the console (**Admin → DLP**) enable the model sidecar and set its URL
to `http://dlp-bert:8000`. See [DLP, capture & audit](dlp-capture-audit.md) and
[`deploy/dlp-bert/README.md`](../deploy/dlp-bert/README.md).

## Run from source

```sh
make build
DATABASE_URL='postgres://airllm:airllm@127.0.0.1:55432/airllm?sslmode=disable' \
REDIS_URL='redis://127.0.0.1:56379/0' \
HTTP_ADDR='127.0.0.1:8099' ENV=dev AUTH_MODE=local \
CAPTURE_BLOB_DIR="$(mktemp -d)" \
./bin/ipsupport-airllm
```

See [Configuration](configuration.md) for every variable.
