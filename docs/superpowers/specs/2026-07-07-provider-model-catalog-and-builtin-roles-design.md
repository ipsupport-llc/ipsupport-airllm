# Provider Model Catalog + Built-in Roles Bootstrap — Design

**Date:** 2026-07-07
**Status:** approved

Two changes, one branch. Part 1 removes model-name guesswork when building
routes: the admin UI offers the upstream provider's live model list. Part 2
fixes a production bug: the built-in roles exist only in the dev seed, so a
prod deployment cannot edit users ("unknown role \"airllm_admin\"") and
operator-created roles start from scratch.

## Part 1 — Provider model catalog (live proxy + micro-cache)

### Problem

`alias_targets.upstream_model` must exactly match the provider's model id.
Today the operator types it blind; a typo surfaces only at request time as an
upstream 4xx. Every supported provider kind (openai, openrouter, xai, ollama)
is OpenAI-compatible and exposes `GET {base_url}/models` with the same API key
the gateway already stores sealed.

### Decision (chosen over DB cache / boot-time fetch)

Live proxy with a short in-memory cache. No migration, no background jobs, no
staleness beyond the TTL. The admin UI fetches the list when the operator
edits an alias; manual entry remains the fallback.

### Backend

**`internal/providers` — optional capability interface:**

```go
// ModelLister is implemented by providers that can enumerate their
// upstream model ids.
type ModelLister interface {
    ListModels(ctx context.Context) ([]string, error)
}
```

- `OpenAICompat.ListModels`: `GET {baseURL}/models` with the stored API key
  (`Authorization: Bearer`), decode `{"data":[{"id":...}]}`, return sorted,
  de-duplicated ids. Non-200 → `*Error{Status: <upstream status>}`.
- `Mock.ListModels`: fixed list `["mock-gpt", "mock-large", "mock-small"]`
  (deterministic; keeps dev and handler tests hermetic).

**`internal/httpapi` — admin endpoint:**

```
GET /api/admin/providers/{name}/models
  200 {"models": ["gpt-4o", ...]}                  — success (possibly cached)
  200 {"models": [], "unsupported": true}          — provider kind cannot list
  404 {"error": ...}                               — provider name not in registry
  502 {"error": "upstream list models: ..."}       — upstream call failed
```

- Admin-gated like the other `/api/admin/*` routes.
- Resolves the provider from the live registry (`s.registry.Get(name)`), so a
  just-saved provider works after the registry reload that the save already
  triggers.
- Type-asserts `ModelLister`; providers that do not implement it (future
  kinds) return the `unsupported` shape rather than an error.

**Micro-cache:**

- Per-provider-name in-memory entry `{models []string, fetchedAt time.Time}`
  guarded by a mutex, TTL 5 minutes (constant `modelCatalogTTL`).
- Only successful fetches are cached; errors are never cached (a transient
  upstream failure must not stick for 5 minutes).
- Upstream fetch uses a 10-second timeout derived from the request context.
- Cache lives on the `Server` struct; a provider save does not need to
  invalidate it (worst case: 5 minutes of stale list, manual entry covers it).

### Frontend (`web/static/app.js`, alias editor)

- Each target row's `upstream model` input gains `list="al-models-<rowid>"`
  plus a `<datalist>` populated per row.
- On row render and on provider `<select>` change, fetch
  `/api/admin/providers/{name}/models` and fill the datalist. Responses are
  memoized per provider name for the lifetime of the modal (no repeated calls
  while adding rows).
- Fetch failure or `unsupported: true` → empty datalist, no toast, input works
  as plain text exactly as today.

### Testing

- `OpenAICompat.ListModels` against `httptest.Server`: success (sorted,
  deduped), non-200 (error carries upstream status), malformed JSON (error).
- Handler tests: success via mock provider; second request within TTL does not
  hit the upstream (counting fake lister); unknown provider name → 404;
  lister error → 502; non-admin session → 403 (matches existing admin-gate
  tests).

## Part 2 — Built-in roles ensured at boot (prod bug fix)

### Problem

`EnsureBootstrapAdmin` creates the first admin with role `airllm_admin`
(`auth.AdminRole`), but `roles_policy` rows are seeded only when
`ENV=dev && AUTH_MODE=local` (`seed.Dev`). In prod the table starts empty, so:

- editing any user that holds `airllm_admin` fails validation
  ("unknown role") because `knownRoles` reads `roles_policy`;
- there is no admin policy row, so admin-role API keys have no
  `allowed_models` grant.

### Fix

New `seed.EnsureBuiltinRoles(ctx, st)` called from `cmd/ipsupport-airllm/main.go`
right after migrations, in **every** env and auth mode:

```sql
INSERT INTO roles_policy (role, allowed_models, allow_passthrough, limits)
VALUES ('airllm_admin', ARRAY['*'], true, '{}'::jsonb)
ON CONFLICT (role) DO NOTHING;

INSERT INTO roles_policy (role, allowed_models, allow_passthrough, limits)
VALUES ('airllm_auditor', ARRAY[]::text[], false, '{}'::jsonb)
ON CONFLICT (role) DO NOTHING;
```

- Role names come from `auth.AdminRole` / `auth.AuditorRole` constants, not
  string literals.
- `ON CONFLICT DO NOTHING`: an operator who tightened `airllm_admin` keeps
  their version; this only guarantees existence.
- `seed.Dev` keeps its inserts (they become no-ops for these two roles) and
  remains the only place the demo `airllm_user` role is created.

### Testing

- Unit test against the test store: fresh DB → both rows exist with expected
  policy; pre-existing modified `airllm_admin` row → untouched after a second
  run (idempotent, non-clobbering).

## Out of scope

- No change to the client-facing `GET /v1/models` (it lists aliases by
  design).
- No background refresh, no DB persistence of the catalog.
- No model metadata (context window, pricing) — ids only.
- No auto-creation of the demo `airllm_user` role outside dev.

## Verification (live, after deploy)

- Admin UI → aliases → New alias → pick provider → datalist offers upstream
  ids.
- Fresh prod-like boot (`ENV=prod`, empty DB) → editing the bootstrap admin
  user succeeds; `GET /api/admin/roles` shows `airllm_admin` and
  `airllm_auditor`.
