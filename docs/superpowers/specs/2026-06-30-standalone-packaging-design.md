# Standalone packaging (P4) — design

**Goal:** Make AirLLM a turnkey self-hosted product: a production docker-compose stack
(bundled Postgres + Redis + gateway, persistent, `ENV=prod`, secrets from `.env`,
permanent admin), a one-command secrets generator, an opt-in auto-HTTPS reverse proxy,
and a standalone deployment guide — distinct from the existing dev/mock compose.

Sub-project **P4** of the deploy-as-product program (P1 auth ✅ · P2 observability ✅ ·
P3 BERT-scale ✅ · **P4 standalone packaging** · P5 Helm/ArgoCD ✅). Built after P5 at the
operator's direction.

## Decisions (locked with the operator)

- **Reverse proxy / TLS:** bundle **Caddy as an opt-in `tls` profile**. The default stack
  binds the app to loopback (safe on any host, incl. a public-IP one); `--profile tls`
  adds Caddy with automatic Let's Encrypt HTTPS, driven by `DOMAIN` + `ACME_EMAIL`.
- **Secrets:** a **`make gen-secrets`** target writes `deploy/.env` with a freshly
  generated master key, admin password, and Postgres password (aborts if `.env` exists).

## What's new vs. the dev compose

`deploy/docker-compose.yml` stays the **dev/mock** stack (`ENV=dev`, `AUTH_MODE=mock`,
dev seed + fixed demo key, ephemeral). P4 adds a separate **`deploy/compose.prod.yaml`**:
`ENV=prod` (master key required, **no dev seed / no fixed key**), `AUTH_MODE=local` with a
**permanent admin** (`AIRLLM_ADMIN_PASSWORD` survives restarts), persistent volumes,
restart policies, healthchecks, and datastores **not published** (internal network only).

## `deploy/compose.prod.yaml`

| Service | Notes |
|---------|-------|
| `postgres` | `postgres:16-alpine`; password `${POSTGRES_PASSWORD}`; **named volume** `pgdata`; healthcheck; `restart: unless-stopped`; **no host port** (internal only) |
| `redis` | `redis:7-alpine` with `--save` (AOF/RDB) on a **named volume** `redisdata`; healthcheck; restart; **no host port** |
| `app` | built from `deploy/Dockerfile`; `ENV=prod`, `AUTH_MODE=local`; `DATABASE_URL`/`REDIS_URL` point at the internal services; `AIRLLM_MASTER_KEY`, `AIRLLM_ADMIN_PASSWORD`, `AIRLLM_ADMIN_USERNAME` from `.env`; `CAPTURE_BLOB_DIR=/var/lib/airllm/captures` on a **named volume** `captures`; healthcheck `/healthz`; restart; published on **`${APP_BIND:-127.0.0.1:8080}`** (loopback by default); `depends_on` both datastores healthy |
| `caddy` (profile `tls`) | `caddy:2-alpine`; mounts `deploy/caddy/Caddyfile`; `DOMAIN`+`ACME_EMAIL` from `.env`; reverse-proxies to `app:8080`; named volumes `caddy_data`/`caddy_config` (certs); ports `80`+`443` (public — required for ACME + serving); restart |
| `dlp-bert` (profile `bert`) | the real BERT sidecar (`deploy/dlp-bert`); internal only; restart. Opt-in (heavy) |

Volumes: `pgdata`, `redisdata`, `captures`, `caddy_data`, `caddy_config` (all named — no
host binds, so no data/certs land in the repo). One default network.

When `--profile tls` is used, set `APP_BIND` so the app is **not** also published on a
public interface (Caddy is the public entry); the app stays reachable internally as
`app:8080`. The guide documents this.

## `deploy/caddy/Caddyfile`

```
{
    email {$ACME_EMAIL}
}
{$DOMAIN} {
    reverse_proxy app:8080
}
```

Caddy obtains and renews certs automatically. `DOMAIN`/`ACME_EMAIL` come from the
environment (compose passes them through). For a private/offline deploy, the guide notes
Caddy's `tls internal` option as an alternative.

## `.env.example` (expanded, prod-capable)

The current `deploy/.env.example` only documents `APP_BIND`. Expand it to cover the prod
stack, every value a placeholder with an `openssl` hint:

```sh
# --- Host bind (loopback by default; never 0.0.0.0 on a public-IP host) ---
APP_BIND=127.0.0.1:8080

# --- Required for ENV=prod (compose.prod.yaml). Generate with `make gen-secrets`. ---
AIRLLM_MASTER_KEY=        # openssl rand -base64 32   (32-byte AES key; rotating it strands sealed creds)
AIRLLM_ADMIN_USERNAME=admin
AIRLLM_ADMIN_PASSWORD=    # openssl rand -base64 24   (permanent admin login)
POSTGRES_PASSWORD=        # openssl rand -hex 24

# --- TLS profile (docker compose --profile tls up) ---
DOMAIN=airllm.example.com
ACME_EMAIL=ops@example.com

# --- Optional: OIDC instead of local auth (AUTH_MODE=oidc) ---
# AUTH_MODE=oidc
# OIDC_ISSUER= … OIDC_CLIENT_ID= … OIDC_CLIENT_SECRET= … OIDC_REDIRECT_URL= … OIDC_ROLES_CLAIM=
```

`AIRLLM_SESSION_KEY` is intentionally omitted — it derives from the master key (HKDF) when
unset; documented as an optional override.

## Make targets

- **`gen-secrets`** — abort if `deploy/.env` exists; else write it from `.env.example`
  with `AIRLLM_MASTER_KEY=$(openssl rand -base64 32)`,
  `AIRLLM_ADMIN_PASSWORD=$(openssl rand -base64 24)`,
  `POSTGRES_PASSWORD=$(openssl rand -hex 24)` filled in, the rest left as placeholders.
  Print the generated admin username/password once.
- **`compose-prod-up`** — `docker compose -f deploy/compose.prod.yaml up -d --build`
  (guards: refuse if `deploy/.env` is missing).
- **`compose-prod-down`** — `docker compose -f deploy/compose.prod.yaml down`
  (keeps volumes; `down -v` documented for a full wipe).

## Permanent admin

With `ENV=prod` + `AUTH_MODE=local`, `EnsureBootstrapAdmin` uses `AIRLLM_ADMIN_PASSWORD`
(idempotent, survives restarts — no random-per-boot password). The guide tells operators
to change it in the console after first login and rotate the `.env` value.

## Docs

A new **"Standalone deployment"** section (in `docs/getting-started.md` or a dedicated
`docs/deployment.md`, linked from the README) covering: `make gen-secrets` → edit `.env`
(domain/email if using TLS) → `make compose-prod-up` → first login as the permanent admin →
optional `--profile tls` and `--profile bert`. Plus a short backups note (`pg_dump` of the
`postgres` service; the named volumes). `docs/configuration.md` cross-references the new
env vars; `docs/operations.md` links the prod compose alongside the k8s/Helm path.

## Verification

- `docker compose -f deploy/compose.prod.yaml config` validates; `--profile tls` and
  `--profile bert` each `config`-validate (services + volumes + the Caddy mount present,
  datastores have **no** host ports).
- `make gen-secrets` produces a valid `.env` in a temp dir (correct key lengths; refuses to
  overwrite an existing file) — tested without touching the real `deploy/.env`.
- **Core prod stack live-verify (loopback, safe on this host):** `compose-prod-up` with a
  generated `.env`; confirm the app boots with `ENV=prod` (master key **required**, **no**
  dev-seed/fixed-key log line), the permanent admin logs in, a restart keeps the same admin
  + data (persistence), and PG/Redis are not published on the host.
- **TLS profile:** `config`-validated only — auto-HTTPS needs a real public domain + open
  80/443, and this host is public-IP/loopback-restricted, so Caddy is **not** brought up
  here (documented).

## Public-clean

`.env.example` carries placeholders + `openssl` hints only; real `deploy/.env` is
git-ignored (`.gitignore` already ignores `.env`/`.env.*` except `.env.example`). Caddy
certs live in named volumes, never the repo. `airllm.example.com`/`ops@example.com` are
placeholders. No secrets, real hosts, or IPs committed.

## Out of scope (P4)

- Bundling Prometheus/Grafana in the prod compose (use the existing dev `--profile metrics`
  or the cluster's stack via the Helm chart).
- A systemd/non-Docker install path, or OS packages.
- Automated backups/restore tooling beyond a documented `pg_dump`.
- Secret managers (Vault/etc.) for the compose path — that's the k8s/Helm story (P5).
