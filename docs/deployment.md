# Standalone deployment (Docker Compose)

A turnkey, self-hosted production deployment of AirLLM: the gateway plus a bundled
Postgres and Redis, running in production mode with persistent storage and a permanent
admin. This is distinct from the **dev/mock** stack (`make compose-up`,
`deploy/docker-compose.yml`), which uses a throwaway database and a fixed demo key.

> **Prerequisites:** Docker + Docker Compose v2, and `openssl` (for `make gen-secrets`).

## 1. Generate secrets

```sh
make gen-secrets
```

This writes `deploy/.env` (git-ignored) with a freshly generated `AIRLLM_MASTER_KEY`,
`AIRLLM_ADMIN_PASSWORD`, and `POSTGRES_PASSWORD`, and prints the admin password once.
It **refuses to overwrite** an existing `deploy/.env`. (No `make`? Copy
`deploy/.env.example` to `deploy/.env` and fill the values with the `openssl` commands
shown inline.)

**Keep `AIRLLM_MASTER_KEY`.** It seals provider credentials; rotating it strands
everything sealed with the old key.

## 2. Bring up the stack

```sh
make compose-prod-up      # docker compose -f deploy/compose.prod.yaml up -d --build
```

Postgres, Redis, and the gateway start with `restart: unless-stopped` and persistent
named volumes (`pgdata`, `redisdata`, `captures`). The datastores are **not published**
on the host — only the gateway is, on `127.0.0.1:8080` by default (override with
`APP_BIND` in `deploy/.env`; never bind `0.0.0.0` on a public-IP host without a proxy).

In production mode (`ENV=prod`) there is **no dev seed and no fixed demo key** — the
only way in is the admin account.

## 3. First login

Open the bound address (default `http://127.0.0.1:8080`), sign in as `admin` with the
generated password, **change it in the console**, then create API keys and add providers.
The admin user persists across restarts. `AIRLLM_ADMIN_PASSWORD` is used **only** to
create the admin on first boot (and to re-create it if the database is wiped); after that,
the env value is ignored — rotate the password in the console.

## 4. Optional: automatic HTTPS (`tls` profile)

For a public deployment with a domain, add Caddy (automatic Let's Encrypt certificates):

```sh
# in deploy/.env: DOMAIN=your.domain  ACME_EMAIL=you@example.com
docker compose -f deploy/compose.prod.yaml --profile tls up -d
```

Caddy serves `:80`/`:443` and reverse-proxies to the gateway. Point your domain's DNS at
the host and open ports 80/443. Keep `APP_BIND` on loopback so the gateway is reachable
only through Caddy. For a private/offline host, use Caddy's `tls internal` (see
`deploy/caddy/Caddyfile`).

## 5. Optional: DLP BERT sidecar (`bert` profile)

```sh
docker compose -f deploy/compose.prod.yaml --profile bert up -d
# then set Sidecar URL = http://dlp-bert:8000 under Admin -> DLP
# scale it:  docker compose -f deploy/compose.prod.yaml --profile bert up -d --scale dlp-bert=3
```

(On kubernetes, use the Helm chart's autoscaling instead — see
[Operations → Kubernetes (Helm chart)](operations.md#kubernetes-helm-chart).)

## Backups

The state lives in Postgres (durable) and the `captures` volume (audit blobs; only when
capture is enabled). Back up Postgres with `pg_dump`:

```sh
docker compose -f deploy/compose.prod.yaml exec postgres \
  pg_dump -U airllm airllm > airllm-$(date +%F).sql
```

Redis holds only rate-limit counters (rebuildable), so it does not need backups.

## Upgrades

Pull the new code, then `make compose-prod-up` again — the image rebuilds and migrations
apply automatically on boot (idempotent, ordered). Volumes are preserved.

## Teardown

```sh
make compose-prod-down                                   # stop, keep data
docker compose -f deploy/compose.prod.yaml down -v       # stop AND delete all volumes
```
