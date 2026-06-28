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

## Phase 2 — Anthropic ingress + passthrough  ✅ DONE (2026-06-27)
- [x] internal/anthropic: codec (decode request→IR incl. system/blocks/tool_use/tool_result; encode IR→Anthropic resp) + SSE StreamWriter
- [x] internal/gateway: POST /v1/messages (non-stream + SSE) via IR → mock; x-api-key supported
- [x] Anthropic-shaped errors (writeProtocolError picks shape by path) for /v1/messages
- [x] mock Anthropic-shaped responses through IR (true byte-passthrough is a real-provider concern → Phase 3)
- [x] Verify: anthropic 401 shape; non-stream msg; SSE event sequence (message_start..message_stop); tool_use stream; ledger anthropic rows; go test (anthropic pkg) green

## Phase 3 — Routing + cross-protocol translation  ✅ DONE (2026-06-27)
- [x] internal/routing: alias catalog → ordered targets + fallback; explicit provider/model passthrough (policy-gated)
- [x] internal/policy: KeyPolicy (allowed_models + allow_passthrough); per-key model gate (403) in both ingresses
- [x] providers.Error (retryable) + mock fail trigger ("fail" model); fallback executor (runChat/runStream, lazy-header so stream falls back pre-first-byte)
- [x] registry built from DB providers (mock-backed); ProviderNames in store
- [x] translation = IR codecs (no redundant translate pkg); caveats documented in docs/translation.md (router prefers same-protocol target)
- [x] Verify e2e: normal alias 200; fallback alias served by secondary (ledger=mock-model-1); explicit passthrough 200; unknown model/provider 404; streaming fallback recovers; go test (policy+providers) green

## Phase 4 — Limits + metering  ✅ DONE (2026-06-27)
- [x] internal/limits: Redis time-bucket counters (5-min) per key×unit; rolling window sums (5h/24h/7d) for tokens + cost(micro-USD); prune + TTL; injectable clock; fail-open on Redis error
- [x] internal/pricing: in-memory price table (loaded from DB) → CostMicroUSD; seed prices for mock models
- [x] policy.Limits + ParseLimits; check-before (429) in both ingresses; increment-after via finalizeUsage (cost in ledger.cost_usd)
- [x] Verify e2e: ledger cost_usd non-zero (0.000044 for 9/26 tok @ 0.5/1.5); first req 200 then second 429 with clear message "tokens over 5h (31 used, 5 cap)"; unit tests SumWindows/BucketStamp/expiredFields green

## Phase 5 — Control-plane API (mock auth, no OIDC)  ✅ DONE (2026-06-27)
- [x] internal/auth: Authenticator + LoginProvider ifaces; Mock = username/password login with RANDOM passwords (admin + operator) logged at boot + HMAC-signed session cookie (real admin login for pre-deploy testing, per operator)
- [x] httpapi session/RBAC: requireSession (cookie → ensureUser) + requireAdmin; /auth/login + /auth/logout
- [x] self-service: /api/me, /api/keys (GET/POST show-once/{id}/revoke), /api/usage (ledger window agg); key snapshots caller's merged role policy
- [x] admin: users, keys(+revoke), usage, audit, roles(GET/PUT), providers(GET/PUT), aliases(GET/PUT/DELETE), pricing(GET/PUT) — all RBAC-gated; mutations write audit_log
- [x] SECURITY: docker-compose ports rebound to 127.0.0.1 (public IP host); dev server binds 127.0.0.1
- [x] Verify e2e: 401 unauth; admin login→cookie→create key→use on /v1 (200); usage; operator 403 vs admin 200; PUT role + audit; auth unit tests green
- NOTE: provider credential (encrypted) admin CRUD still deferred (creds storage pending); aliases/providers/pricing CRUD present

## Phase 6 — Frontend SPA  ✅ DONE (2026-06-27)
- [x] web/static SPA — DEVIATION: vanilla HTML/CSS/JS (not SvelteKit) for the mock: robust go:embed (always builds, no node stage / no npm-before-go-build), clean repo (no node_modules/artifacts), faster in the loop. Same architecture intent (SPA, talks only to /api, served by Go). Dash design tokens (stone/indigo dark). SvelteKit swappable later.
- [x] pages: Login (mock pw), Dashboard (usage cards + key count), API Keys (create show-once/list/revoke), Usage, Admin (users/keys/usage/roles/aliases/providers/pricing/audit) with edit modals + RBAC-aware nav
- [x] web/embed.go (go:embed) + httpapi/spa.go: catch-all GET "/" serves SPA with index fallback; /api,/v1,/auth excluded → JSON 404
- [x] Verify: served / (html), /app.css, /app.js (correct content-types); /api/me→401 JSON, /v1/*→404 JSON, deep routes→SPA fallback; API endpoints wired to Phase-5 control-plane. Browser click-through = operator's manual test (login pw in logs).

## Phase 7 — Hardening + docs  ✅ DONE (2026-06-27)
- [x] audit log writes on control-plane mutations (done in P5: role/provider/alias/pricing/key.revoke)
- [x] README quickstart (compose up → get pw from logs → console → create key → curl /v1 → usage) + config table + mock behaviours + loopback note
- [x] seed: demo role policy / aliases (mock-gpt + mock-fallback) / mock provider / pricing / fixed dev key — idempotent on boot (done in P5)
- [x] Verify: `docker compose down -v` then `up --build` from clean state → app image builds (go:embed), boots/migrates/seeds, console served, login→create key→/v1 200→/v1/messages 200→usage. FULL STACK e2e green.

## Phase 8 — Review gates (the "done" bar)  ✅ DONE (2026-06-27)
- [x] Readiness 1/5 — build/vet/test/gofmt green (7 pkgs)
- [x] Readiness 2/5 — clean-state `compose down -v && up --build` full e2e
- [x] Readiness 3/5 — repo hygiene: NO Cyrillic anywhere, no secrets, no artifacts committed, status clean
- [x] Readiness 4/5 — spec coverage: dual ingress, routing/fallback/passthrough, policy, limits+metering, control-plane+RBAC, console (OIDC + real providers deferred to k8s by design)
- [x] Readiness 5/5 — operability: README quickstart accurate, compose loopback-only
- [x] Self review 1/5 (correctness) + fixes: /v1/models now filtered by key policy; seeded airouter_user role (restricted to mock-gpt) — verified role-based model gate (operator: mock-gpt 200, mock-fallback 403)
- [x] Self review 2/5 (security) + fix: 16 MiB request-body cap (MaxBytesReader) — verified 17MB → 400, app survives; authz/RBAC + key-hash confirmed
- [x] Self review 3/5 (simplification): code already DRY (chatEntry/usageWindows/panelTable/modalForm); no risky refactor (surgical)
- [x] Self review 4/5 (concurrency/leaks): 13 Query ↔ 13 defer rows.Close(); single goroutine; pricing.Table RWMutex; fail-open limiter
- [x] Self review 5/5 (final): hygiene re-scan clean; final smoke green (chat/stream/messages/401/404/body-limit)

## DONE — full local mock complete + 5 readiness + 5 self-review rounds (2026-06-27)
