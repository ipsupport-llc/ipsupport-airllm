# ipsupport-airouter — design

Self-hosted, OIDC-governed LLM gateway. One Go service exposes
OpenAI- and Anthropic-compatible endpoints to internal coding agents,
authenticates them by API key, enforces per-key model policy and rolling
usage limits, routes to upstream providers (OpenAI, OpenRouter, xAI,
Anthropic), and meters tokens/cost. A SvelteKit SPA (served by the same
binary) provides self-service key management and admin policy/analytics.

- **Status:** design approved 2026-06-27, pre-implementation.
- **Repo:** `github.com/rromenskyi/ipsupport-airouter` (private now,
  written public-grade, intended to open-source later).
- **License:** Apache-2.0.
- **Language of the repo:** English only — code, comments, commit
  messages, docs, UI strings. No Cyrillic anywhere in the tree.

## Goals

- Single OpenAI/Anthropic-compatible endpoint for employees' coding agents
  (Cursor, Cline, Aider, Claude Code, Zed, …) with minimal feature loss.
- Govern *which models* each employee may use, by their OIDC role.
- Cap usage per API key over rolling 5h / 24h / 7d windows, in tokens
  and/or USD.
- Route across multiple upstream providers with fallback.
- Self-service key lifecycle for employees; full oversight for admins.
- Public-grade hygiene: no secrets in git, pluggable identity, clean
  history, permissive license.

## Non-goals (v1)

- Billing/invoicing (we meter and report; we do not invoice).
- Multi-tenant isolation beyond OIDC roles.
- Automatic price-list ingestion from provider APIs (pricing is config /
  seed data with manual override).
- `/v1/responses` ingress (deferred; `/v1/chat/completions`,
  `/v1/models`, `/v1/messages` ship in v1).
- Cluster/infra provisioning from this repo (done via a handoff to the
  `platform` agent).

## Architecture

One Go binary with two auth planes plus the static SPA it serves.

```
                      ipsupport-airouter (Go)
  ┌───────────────────────────────────────────────────────────────┐
  │  control-plane  (browser, OIDC session cookie)                 │
  │    /auth/* (OIDC login/callback/logout)                        │
  │    /api/*  (REST/JSON for the SPA: keys, policy, usage, admin) │
  │    /        (SvelteKit SPA, adapter-static, embedded/served)   │
  │                                                                │
  │  data-plane     (coding agents, Bearer API key)               │
  │    /v1/chat/completions   /v1/models     (OpenAI ingress)      │
  │    /v1/messages                          (Anthropic ingress)   │
  │       → authn(key) → policy → limits(check) → router →         │
  │         provider(passthrough|translate) → meter → ledger       │
  └───────────────────────────────────────────────────────────────┘
        │                                   │
   Postgres (durable state)           Redis (rolling counters)
```

The SPA never touches a database — it calls `/api/*` only. Coding agents
touch `/v1/*` only. Cluster mutations are out of scope for the running
service.

### Components (`/internal`)

Each unit has one purpose and a narrow interface so it can be tested in
isolation.

- `config` — load/validate config from env + optional file; atomic,
  abort-on-corrupt for any read-modify-write of on-disk state.
- `oidc` — generic OIDC: discovery by issuer, auth-code+PKCE flow,
  session cookies, configurable `roles_claim` / `groups_claim` JSON
  paths. Zitadel is one config; Okta/Auth0/Keycloak/Google work by
  config alone. No provider name hardcoded.
- `apikey` — generate (`air_<env>_<random>`), hash (sha256), verify,
  store prefix + last-4 + metadata; rotate/revoke.
- `policy` — role → policy resolution (allowed models/aliases, default
  limits, explicit-passthrough permission); snapshot a policy onto a key
  at issue time; rebuild snapshots when a role policy changes.
- `gateway` — HTTP handlers for the data-plane ingress endpoints.
- `providers` — `Provider` interface + one implementation per upstream
  (OpenAI, OpenRouter, xAI, Anthropic). Adding a provider = one file.
- `routing` — resolve a requested model: alias-catalog → ordered target
  list with fallback; or explicit `provider/model` passthrough when the
  key's policy permits. Prefer a same-protocol target to avoid
  translation loss.
- `translate` — Anthropic ↔ OpenAI conversion for the common path:
  messages, system, tools/tool_choice, streaming deltas, stop reasons,
  usage. Documents which features degrade across protocols.
- `limits` — rolling-window check + increment over Redis buckets.
- `ledger` — per-request usage record into Postgres (source of truth for
  reporting/reconciliation).
- `store` — Postgres (pgx) and Redis (go-redis) access; migrations.
- `httpapi` — control-plane REST for the SPA (keys, policy, usage,
  admin) with OIDC session + RBAC gating.

## Ingress protocols and translation

- **Ingress:** OpenAI (`/v1/chat/completions`, `/v1/models`) and
  Anthropic (`/v1/messages`).
- **Principle — minimize feature loss:** when the client protocol equals
  the chosen upstream protocol, do a **byte-level streaming passthrough**
  (preserves tool calls, reasoning blocks, prompt-caching, provider
  extras). Translate only on cross-protocol routing.
- **Translation matrix:**
  | ingress → upstream | behavior |
  |---|---|
  | OpenAI → OpenAI-shaped (OpenAI/OpenRouter/xAI) | passthrough |
  | Anthropic → Anthropic-direct | passthrough |
  | Anthropic → OpenAI-shaped (e.g. Claude Code → Grok) | translate |
  | OpenAI → Anthropic-direct | translate |
- **Documented caveats:** cross-protocol translation may not carry
  provider-specific features (Anthropic prompt-caching, OpenAI logprobs,
  reasoning controls). The router prefers a same-protocol target; the UI
  and docs surface when a route is translated.

## Identity, roles, policy (control-plane)

- Generic OIDC login for the browser admin/self-service UI; server-side
  session cookie. Roles/groups extracted via configurable claim paths.
- A **role policy** holds: allowed aliases/models, default limits (per
  5h/24h/7d window, in tokens and/or USD), and whether
  explicit-passthrough is allowed.
- RBAC: at minimum `airouter_admin` (manage everything) and
  `airouter_user` (self-service own keys, view own usage). Admin role and
  claim paths are configurable.

## API keys (self-service + admin)

- Format `air_<env>_<random>`; shown **once** at creation; stored as
  sha256 hash + visible prefix + last-4 + metadata.
- Employee: create/rotate/revoke their own keys; each key snapshots the
  owner's role policy (allowed models + default limits).
- Admin: list/manage all keys, override per-key limits, revoke.
- Request path: key → snapshot policy → model check → limits check →
  route.

## Limits & metering (data-plane, per key)

- **Unit:** tokens (primary) and/or USD (secondary, via a pricing table
  model → $/1M input, $/1M output). A limit may be set in either unit per
  window. Configurable.
- **Windows:** rolling 5h / 24h / 7d, implemented as Redis time buckets
  (e.g. 5-minute buckets with TTL, summed over the window) for bounded
  memory. The Postgres `usage_ledger` is the source of truth for reports
  and reconciliation.
- **Enforcement order:** *check-before, increment-after*. Reject with a
  clear `429` body if a window is already exceeded; increment counters
  from the actual upstream `usage` after the response completes. A single
  request may slightly overshoot a window — an accepted trade-off.
- **Usage source:** read `usage` from the upstream response; for streams
  use OpenAI `stream_options.include_usage` and Anthropic
  `message_delta.usage`. Where absent, best-effort tokenizer estimate
  (flagged as estimated).

## Providers & upstream credentials

- v1 providers: OpenAI, OpenRouter, xAI (Grok), Anthropic-direct.
- Upstream credentials are stored **encrypted in Postgres** (AES-GCM).
  The **master encryption key lives in env** (Vault later) — never in the
  same database, never in git. Admins manage provider credentials from
  the UI.

## Data model (Postgres)

- `users` — OIDC subject, email, display, roles snapshot, timestamps.
- `api_keys` — id, owner (user), hash, prefix, last4, policy snapshot
  (allowed models, limits, passthrough flag), status, timestamps.
- `roles_policy` — role → allowed aliases/models, default limits,
  passthrough flag.
- `model_aliases` + `alias_targets` — alias → ordered (provider,
  upstream_model) targets with fallback order.
- `providers` — provider id, kind, base_url, encrypted credentials,
  enabled.
- `pricing` — model → input/output $ per 1M tokens.
- `usage_ledger` — one row per request: key_id, user, alias, resolved
  provider+model, ingress/upstream protocol, prompt/completion tokens,
  cost_usd, status, latency_ms, error, ts.
- `audit_log` — control-plane mutations (who/what/when).

Redis: rolling-window counters keyed by key × window × unit.
Migrations via golang-migrate (or goose).

## Frontend (SvelteKit SPA)

Reuse from `platform-dash`: design tokens in `app.css` (stone/indigo,
dark/light), and components ConfirmDialog, Toasts, LiveDot, QuickSearch.
SPA build (adapter-static) served by the Go binary; talks only to
`/api/*`.

Pages:
- **Login** (OIDC redirect).
- **My Dashboard** — own usage summary + own keys.
- **Keys** — create (show-once), rotate, revoke.
- **Usage** — charts: tokens and $ over time, by model/alias.
- **Admin** — Roles & Policies, Models/Aliases, Providers, Users & Keys,
  Org Usage, Audit.

## Repo layout

```
/cmd/ipsupport-airouter        main entrypoint
/internal/{config,oidc,apikey,policy,gateway,providers,routing,
           translate,limits,ledger,store,httpapi}
/web                           SvelteKit SPA (adapter-static)
/migrations                    SQL migrations
/deploy                        Dockerfile (k8s via platform handoff)
/docs                          design + operator docs
README.md  LICENSE (Apache-2.0)
```

Stack: Go (chi or stdlib net/http), pgx, go-redis, coreos/go-oidc +
golang.org/x/oauth2, golang-migrate. Dependencies kept lean. CI mirrors
`platform-dash` (GHCR, self-hosted pd-runners).

## Security & operational notes

- No secrets in git; master key + OIDC client secret via env (Vault
  later).
- API keys hashed at rest; shown once.
- Config writes atomic with abort-on-corrupt.
- Cluster resources (namespace, ingress, the env secrets) requested via a
  handoff to the `platform` agent — never applied from this repo's
  session.

## Suggested phasing (refined in the implementation plan)

1. Skeleton: module, config, Postgres/Redis wiring, migrations, health.
2. Data-plane MVP: OpenAI ingress → OpenAI/OpenRouter/xAI passthrough +
   API-key authn + ledger (no limits yet).
3. Anthropic ingress + Anthropic-direct passthrough.
4. Cross-protocol translator (common path) + routing aliases/fallback.
5. Limits (Redis rolling buckets, check-before/increment-after).
6. Control-plane: OIDC + role policy + self-service keys (REST).
7. SPA: keys, usage, admin.
8. Hardening: pricing/cost, audit, docs, deploy handoff.

## Open questions / deferred

- `/v1/responses` ingress — deferred to a later version.
- Vault-backed master key & provider creds — deferred (env for now).
- Tokenizer choice for best-effort estimates when upstream omits usage.
- Whether to embed the SPA into the Go binary (go:embed) or ship as a
  sidecar static dir — decided in the implementation plan.
