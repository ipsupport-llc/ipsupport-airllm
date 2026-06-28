# ipsupport-airouter — implementation plan (full mock)

Goal: a complete, locally-runnable **mock** of the gateway — real
architecture and pipeline (key-auth → policy → limits → routing →
metering → ledger → SPA), with a **mock upstream provider** (simulated
streaming/tool-calls/usage) and **dev/mock auth instead of OIDC**.
Runnable via docker-compose (Postgres + Redis + app). Real providers and
OIDC are wired later, on the kubernetes deploy.

Definition of done: full mock works end-to-end **plus** 5 readiness
review rounds **plus** 5 self code-review rounds (see bottom).

Tech: Go 1.26, stdlib `net/http` (1.22+ routed mux, no router dep),
`jackc/pgx/v5`, `redis/go-redis/v9`. SvelteKit SPA (adapter-static).
English-only repo. No secrets in git.

## Conventions
- Mark a step `[x]` only after it builds/passes; note evidence in commit.
- Keep packages small and single-purpose; table-driven tests per package.
- `make` targets drive build/test/run; docker-compose for local stack.

---

## Phase 0 — Foundation  ✅ DONE (verified 2026-06-27)
- [x] go.mod (module github.com/rromenskyi/ipsupport-airouter), .gitignore, LICENSE (Apache-2.0), README
- [x] Makefile (build, test, vet, run, compose-up/down, web-build)
- [x] deploy/docker-compose.yml (postgres + redis + app) and Dockerfile  (host ports offset 55432/56379 to avoid collisions)
- [x] internal/config: env-driven config + validation
- [x] internal/store: pgxpool + go-redis wiring; embedded SQL migrator (per-file tx, schema_migrations)
- [x] migrations/0001_init.sql: users, api_keys, roles_policy, model_aliases, alias_targets, providers, pricing, usage_ledger, audit_log
- [x] cmd/ipsupport-airouter/main.go: wire config+store+http server; /healthz, /readyz
- [x] Verify: go build/vet/gofmt clean; app boots against compose; migration applied; /healthz 200; /readyz 200

## Phase 1 — Data-plane MVP (OpenAI ingress → mock upstream)  ✅ DONE (2026-06-27)
- [x] internal/apikey: generate/hash(sha256); prefix + last4 (+ unit tests)
- [x] internal/providers: Provider interface + mock provider (content + usage + streaming + tool-calls)
- [x] internal/llm: provider-neutral IR (+ StreamChunk); internal/openai: request/response + SSE codec
- [x] internal/gateway: POST /v1/chat/completions (non-stream + SSE stream), GET /v1/models
- [x] key-auth middleware (Bearer/x-api-key → key lookup → key on context)
- [x] internal/ledger: write one usage row per request (best-effort), incl. streaming
- [x] internal/seed: dev mock seed (user/role/provider/alias/fixed key)
- [x] Verify: 401/400 paths; non-stream + SSE chat; tool-call stream (tooltest trigger); /v1/models; ledger rows; go test green (openai+providers+apikey)

## Phase 2 — Anthropic ingress + passthrough
- [ ] internal/gateway: POST /v1/messages (non-stream + SSE)
- [ ] mock Anthropic-shaped responses; same-protocol passthrough path
- [ ] Verify: curl /v1/messages (stream + non-stream); ledger rows; tests

## Phase 3 — Routing + cross-protocol translation
- [ ] internal/routing: alias catalog → ordered targets + fallback; explicit provider/model passthrough (policy-gated)
- [ ] internal/translate: Anthropic<->OpenAI (messages/system/tools/tool_choice/stream-deltas/stop-reason/usage); document caveats
- [ ] Verify: alias resolves + fails over to next target on simulated error; cross-protocol translation round-trips in tests

## Phase 4 — Limits + metering
- [ ] internal/limits: Redis rolling buckets per key×window×unit (5h/24h/7d), tokens + USD
- [ ] pricing table seed + cost computation; check-before / increment-after
- [ ] Verify: exceeding a window returns clear 429; counters decay over time; tests with a fake clock

## Phase 5 — Control-plane API (mock auth, no OIDC)
- [ ] internal/auth: pluggable auth iface; mock/dev impl (config-set admin user, no OIDC)
- [ ] internal/httpapi: /api keys (self CRUD + show-once), /api/usage (self), /api/admin/* (roles_policy, aliases, providers, users, keys, org-usage, audit) with RBAC
- [ ] Verify: REST endpoints behave; RBAC denies non-admin; tests

## Phase 6 — Frontend SPA
- [ ] web/ SvelteKit (adapter-static); reuse dash design tokens + ConfirmDialog/Toasts/LiveDot/QuickSearch
- [ ] pages: Login(mock), My Dashboard, Keys, Usage, Admin(policies/aliases/providers/users/usage/audit)
- [ ] Go serves built SPA (go:embed) and /api
- [ ] Verify: `npm run build`; app serves SPA; click-through against the mock works

## Phase 7 — Hardening + docs
- [ ] audit log writes on control-plane mutations
- [ ] README quickstart (compose up → seed → use a key → see usage)
- [ ] seed script: demo role policy, aliases, mock provider, demo key
- [ ] Verify: fresh `compose up` + seed → end-to-end demo works from clean state

## Phase 8 — Review gates (the "done" bar)
- [ ] Readiness review 1/5 — everything ready?
- [ ] Readiness review 2/5
- [ ] Readiness review 3/5
- [ ] Readiness review 4/5
- [ ] Readiness review 5/5
- [ ] Self code-review round 1/5 (correctness/bugs) + fixes
- [ ] Self code-review round 2/5 (security: key handling, injection, authz) + fixes
- [ ] Self code-review round 3/5 (simplification/reuse/altitude) + fixes
- [ ] Self code-review round 4/5 (concurrency/streaming/resource leaks) + fixes
- [ ] Self code-review round 5/5 (final pass, docs, no Cyrillic, no secrets) + fixes
