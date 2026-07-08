# Provider Model Catalog + Built-in Roles Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let the admin UI offer each provider's live upstream model list (no more guessing `upstream_model`), and guarantee the built-in roles exist in every environment (prod bug: "unknown role \"airllm_admin\"").

**Architecture:** A `ModelLister` optional capability on providers, proxied through a new admin endpoint with a 5-minute in-memory cache; the alias editor feeds a `<datalist>` from it. Separately, an idempotent `seed.EnsureBuiltinRoles` runs at boot in every env.

**Tech Stack:** Go 1.26 stdlib only (no new deps), vanilla JS admin SPA.

**Spec:** `docs/superpowers/specs/2026-07-07-provider-model-catalog-and-builtin-roles-design.md`

## Global Constraints

- English only in the repo (code, comments, docs).
- No new Go dependencies; stdlib only.
- No environment-specific values (hostnames, DSNs, org names) anywhere.
- Client-facing `GET /v1/models` is NOT touched (it lists aliases by design).
- Only successful catalog fetches are cached; errors are never cached.
- Role names always via `auth.AdminRole` / `auth.AuditorRole` constants, never string literals.
- `EnsureBuiltinRoles` must be non-clobbering: `ON CONFLICT (role) DO NOTHING`.
- Run `gofmt -l .` before every commit (must print nothing).

---

### Task 1: `ModelLister` capability — `OpenAICompat.ListModels` + `Mock.ListModels`

**Files:**
- Modify: `internal/providers/provider.go` (add `ModelLister` interface after the `Provider` interface, ~line 27)
- Modify: `internal/providers/openai_compat.go` (add `ListModels` method)
- Modify: `internal/providers/mock.go` (add `ListModels` method)
- Test: `internal/providers/list_models_test.go` (new)

**Interfaces:**
- Consumes: existing `OpenAICompat` struct (fields `name, kind, baseURL, apiKey, hc`), `httpError(name string, status int, body []byte) error`, `*Error` type.
- Produces: `type ModelLister interface { ListModels(ctx context.Context) ([]string, error) }`; `(*OpenAICompat).ListModels` and `(*Mock).ListModels` both satisfying it. `Mock` returns exactly `[]string{"mock-gpt", "mock-large", "mock-small"}`. Task 2 type-asserts `entry.Provider.(providers.ModelLister)`.

- [ ] **Step 1: Write the failing tests**

Create `internal/providers/list_models_test.go`:

```go
package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestOpenAICompatListModels(t *testing.T) {
	var gotAuth, gotPath string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]string{
				{"id": "gpt-b"}, {"id": "gpt-a"}, {"id": "gpt-b"},
			},
		})
	}))
	defer ts.Close()

	p := NewOpenAICompat("up", "openai", ts.URL, "sk-test")
	models, err := p.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if want := []string{"gpt-a", "gpt-b"}; !reflect.DeepEqual(models, want) {
		t.Errorf("models = %v, want %v (sorted, deduped)", models, want)
	}
	if gotAuth != "Bearer sk-test" {
		t.Errorf("Authorization = %q, want Bearer sk-test", gotAuth)
	}
	if gotPath != "/models" {
		t.Errorf("path = %q, want /models", gotPath)
	}
}

func TestOpenAICompatListModelsNon200(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"nope"}`, http.StatusUnauthorized)
	}))
	defer ts.Close()

	p := NewOpenAICompat("up", "openai", ts.URL, "bad")
	_, err := p.ListModels(context.Background())
	perr, ok := err.(*Error)
	if !ok {
		t.Fatalf("err = %T (%v), want *Error", err, err)
	}
	if perr.Status != http.StatusUnauthorized {
		t.Errorf("Status = %d, want 401", perr.Status)
	}
}

func TestOpenAICompatListModelsBadJSON(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not json"))
	}))
	defer ts.Close()

	p := NewOpenAICompat("up", "openai", ts.URL, "")
	if _, err := p.ListModels(context.Background()); err == nil {
		t.Fatal("malformed JSON must return an error")
	}
}

func TestMockListModels(t *testing.T) {
	m := NewMock("mock")
	models, err := m.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	want := []string{"mock-gpt", "mock-large", "mock-small"}
	if !reflect.DeepEqual(models, want) {
		t.Errorf("models = %v, want %v", models, want)
	}
}

// Compile-time capability checks.
var (
	_ ModelLister = (*OpenAICompat)(nil)
	_ ModelLister = (*Mock)(nil)
)
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/providers/ -run 'ListModels' -v`
Expected: compile error — `undefined: ModelLister`, `p.ListModels undefined`.

- [ ] **Step 3: Implement**

In `internal/providers/provider.go`, after the `Provider` interface (after its closing brace, ~line 27), add:

```go
// ModelLister is implemented by providers that can enumerate their upstream
// model ids. Providers without a listing endpoint simply do not implement it.
type ModelLister interface {
	ListModels(ctx context.Context) ([]string, error)
}
```

In `internal/providers/openai_compat.go`, add (after `Chat`):

```go
// ListModels fetches GET {base}/models and returns the sorted, de-duplicated
// model ids. All OpenAI-compatible upstreams expose this endpoint.
func (p *OpenAICompat) ListModels(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/models", nil)
	if err != nil {
		return nil, err
	}
	if p.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
	resp, err := p.hc.Do(req)
	if err != nil {
		return nil, &Error{Status: http.StatusBadGateway, Retryable: true, Message: err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, httpError(p.name, resp.StatusCode, b)
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode %s models: %w", p.name, err)
	}
	seen := map[string]bool{}
	var ids []string
	for _, m := range out.Data {
		if m.ID == "" || seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		ids = append(ids, m.ID)
	}
	sort.Strings(ids)
	return ids, nil
}
```

Add `"encoding/json"` and `"sort"` to the imports of `openai_compat.go` (`io`, `fmt`, `net/http` are already imported).

In `internal/providers/mock.go`, add (after `Kind`/`Protocol` methods):

```go
// ListModels returns a fixed, deterministic model list for dev and tests.
func (m *Mock) ListModels(_ context.Context) ([]string, error) {
	return []string{"mock-gpt", "mock-large", "mock-small"}, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/providers/ -v`
Expected: all PASS (including pre-existing tests).

- [ ] **Step 5: Commit**

```bash
gofmt -l . # must print nothing
git add internal/providers/
git commit -m "feat(providers): ModelLister capability on OpenAI-compat and mock providers"
```

---

### Task 2: Admin endpoint `GET /api/admin/providers/{name}/models` with micro-cache

**Files:**
- Create: `internal/httpapi/api_provider_models.go`
- Modify: `internal/httpapi/api_admin.go:28` (register route after `PUT /api/admin/providers/{name}`)
- Modify: `internal/httpapi/server.go:69` (add cache field to `Server` struct, after `modelPool`)
- Modify: `docs/api.md:81` (add the endpoint row after the providers row)
- Test: `internal/httpapi/api_provider_models_test.go` (new)

**Interfaces:**
- Consumes: `providers.ModelLister` (Task 1); `s.reg() *providers.Registry`; `Registry.Get(name) (*Entry, bool)`; `Entry.Provider`; `writeJSON(w, status, v)`; `writeControlError(w, status, msg)`; `s.requireAdmin`; test doubles `fakeAuth` (exists in `api_audit_test.go`, same package).
- Produces: route `GET /api/admin/providers/{name}/models` returning `{"models":[...]}` | `{"models":[],"unsupported":true}` | 404 | 502. Task 4's UI calls it.

- [ ] **Step 1: Write the failing tests**

Create `internal/httpapi/api_provider_models_test.go`:

```go
package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/auth"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/llm"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/providers"
)

// countingLister is a Provider+ModelLister that counts ListModels calls.
type countingLister struct {
	providers.Provider // embeds mock for Chat/ChatStream
	calls              int
	models             []string
	err                error
}

func (c *countingLister) ListModels(_ context.Context) ([]string, error) {
	c.calls++
	if c.err != nil {
		return nil, c.err
	}
	return c.models, nil
}

// bareProvider implements Provider but NOT ModelLister.
type bareProvider struct{ providers.Provider }

func (b *bareProvider) Name() string { return "bare" }

func newModelsTestServer(t *testing.T, reg *providers.Registry) *Server {
	t.Helper()
	s := &Server{
		mux:  http.NewServeMux(),
		auth: &fakeAuth{principal: auth.Principal{Subject: "a", Roles: []string{auth.AdminRole}}},
		ensureUserFn: func(_ context.Context, p auth.Principal) (string, error) {
			return "uid-" + p.Subject, nil
		},
	}
	s.regPtr.Store(reg)
	s.mux.HandleFunc("GET /api/admin/providers/{name}/models",
		s.requireAdmin(s.handleAdminProviderModels))
	return s
}

func getModels(t *testing.T, s *Server, name string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/admin/providers/"+name+"/models", nil)
	rw := httptest.NewRecorder()
	s.mux.ServeHTTP(rw, req)
	var body map[string]any
	json.Unmarshal(rw.Body.Bytes(), &body)
	return rw.Code, body
}

func TestProviderModelsSuccessAndCache(t *testing.T) {
	cl := &countingLister{Provider: providers.NewMock("up"), models: []string{"m-a", "m-b"}}
	reg := providers.NewRegistry()
	reg.Register(cl, 0)
	// countingLister embeds the mock whose Name() is "up".
	s := newModelsTestServer(t, reg)

	code, body := getModels(t, s, "up")
	if code != http.StatusOK {
		t.Fatalf("code = %d, want 200", code)
	}
	got, _ := body["models"].([]any)
	if len(got) != 2 {
		t.Fatalf("models = %v, want 2 entries", body["models"])
	}

	// Second call within TTL must be served from cache.
	getModels(t, s, "up")
	if cl.calls != 1 {
		t.Errorf("upstream calls = %d, want 1 (second call cached)", cl.calls)
	}
}

func TestProviderModelsUnknownProvider(t *testing.T) {
	s := newModelsTestServer(t, providers.NewRegistry())
	code, _ := getModels(t, s, "ghost")
	if code != http.StatusNotFound {
		t.Errorf("code = %d, want 404", code)
	}
}

func TestProviderModelsUnsupportedKind(t *testing.T) {
	reg := providers.NewRegistry()
	reg.Register(&bareProvider{Provider: providers.NewMock("bare")}, 0)
	s := newModelsTestServer(t, reg)
	code, body := getModels(t, s, "bare")
	if code != http.StatusOK {
		t.Fatalf("code = %d, want 200", code)
	}
	if body["unsupported"] != true {
		t.Errorf("unsupported = %v, want true", body["unsupported"])
	}
}

func TestProviderModelsUpstreamErrorNotCached(t *testing.T) {
	cl := &countingLister{Provider: providers.NewMock("up"),
		err: &providers.Error{Status: 500, Message: "boom"}}
	reg := providers.NewRegistry()
	reg.Register(cl, 0)
	s := newModelsTestServer(t, reg)

	code, _ := getModels(t, s, "up")
	if code != http.StatusBadGateway {
		t.Fatalf("code = %d, want 502", code)
	}
	getModels(t, s, "up")
	if cl.calls != 2 {
		t.Errorf("upstream calls = %d, want 2 (errors are never cached)", cl.calls)
	}
}

// Silence unused import if llm is not needed after edits; llm.ChatRequest is
// referenced here to keep the import honest.
var _ = llm.ChatRequest{}
```

Note for the implementer: `countingLister` embeds `providers.Provider` (a `*Mock` named `"up"`), so `Registry.Register` sees `Name() == "up"`. If `Registry.Register` keys off `Provider.Name()` via the outer type, embedding promotes `Name()` correctly. If the `llm` import ends up unused, drop it and the `var _` line.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/httpapi/ -run 'ProviderModels' -v`
Expected: compile error — `s.handleAdminProviderModels undefined`.

- [ ] **Step 3: Implement**

In `internal/httpapi/server.go`, add to the `Server` struct after `modelPool *modelpool.Pool` (line 69):

```go
	catalog catalogCache // per-provider upstream model list micro-cache
```

Create `internal/httpapi/api_provider_models.go`:

```go
package httpapi

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/providers"
)

// modelCatalogTTL bounds staleness of the upstream model list micro-cache.
const modelCatalogTTL = 5 * time.Minute

// modelCatalogTimeout bounds a single upstream list-models call.
const modelCatalogTimeout = 10 * time.Second

type catalogEntry struct {
	models    []string
	fetchedAt time.Time
}

// catalogCache is a tiny TTL cache keyed by provider name. The zero value is
// ready to use.
type catalogCache struct {
	mu      sync.Mutex
	entries map[string]catalogEntry
}

func (c *catalogCache) get(name string, now time.Time) ([]string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[name]
	if !ok || now.Sub(e.fetchedAt) > modelCatalogTTL {
		return nil, false
	}
	return e.models, true
}

func (c *catalogCache) put(name string, models []string, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]catalogEntry{}
	}
	c.entries[name] = catalogEntry{models: models, fetchedAt: now}
}

// handleAdminProviderModels proxies the upstream model list for one provider
// so the alias editor can offer real model ids instead of free-text guessing.
// Only successful fetches are cached (TTL modelCatalogTTL); errors always
// retry upstream.
func (s *Server) handleAdminProviderModels(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	entry, ok := s.reg().Get(name)
	if !ok {
		writeControlError(w, http.StatusNotFound, "provider not found")
		return
	}
	lister, ok := entry.Provider.(providers.ModelLister)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"models": []string{}, "unsupported": true})
		return
	}
	if models, ok := s.catalog.get(name, time.Now()); ok {
		writeJSON(w, http.StatusOK, map[string]any{"models": models})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), modelCatalogTimeout)
	defer cancel()
	models, err := lister.ListModels(ctx)
	if err != nil {
		writeControlError(w, http.StatusBadGateway, "upstream list models: "+err.Error())
		return
	}
	if models == nil {
		models = []string{}
	}
	s.catalog.put(name, models, time.Now())
	writeJSON(w, http.StatusOK, map[string]any{"models": models})
}
```

In `internal/httpapi/api_admin.go`, after the `PUT /api/admin/providers/{name}` registration (line 28), add:

```go
	s.mux.HandleFunc("GET /api/admin/providers/{name}/models", a(s.handleAdminProviderModels))
```

In `docs/api.md`, after the providers row (line 81), add:

```markdown
| `GET` | `/api/admin/providers/{name}/models` | Live upstream model ids for one provider (5-min cache; `unsupported: true` when the kind cannot list) |
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/httpapi/ -run 'ProviderModels' -v && go test ./...`
Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -l . # must print nothing
git add internal/httpapi/ docs/api.md
git commit -m "feat(admin): GET /api/admin/providers/{name}/models with 5-min micro-cache"
```

---

### Task 3: Built-in roles ensured at boot

**Files:**
- Create: `internal/seed/builtin.go`
- Modify: `cmd/ipsupport-airllm/main.go:57-59` (call after `st.Migrate`)
- Test: `internal/seed/builtin_test.go` (new)

**Interfaces:**
- Consumes: `store.Store` (field `PG` with `Exec`), `auth.AdminRole` (= `"airllm_admin"`), `auth.AuditorRole` constants.
- Produces: `seed.EnsureBuiltinRoles(ctx context.Context, st *store.Store) error` and pure `seed.BuiltinRoles() []BuiltinRole`.

- [ ] **Step 1: Write the failing test**

There is no PG test harness in this repo (all tests are unit-level), so the
policy data is exposed as a pure function and tested directly; the SQL loop is
trivial and covered by the live compose verification at the end of the plan.

Create `internal/seed/builtin_test.go`:

```go
package seed

import (
	"testing"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/auth"
)

func TestBuiltinRolesPolicy(t *testing.T) {
	roles := BuiltinRoles()
	byName := map[string]BuiltinRole{}
	for _, r := range roles {
		byName[r.Role] = r
	}

	admin, ok := byName[auth.AdminRole]
	if !ok {
		t.Fatalf("BuiltinRoles missing %q", auth.AdminRole)
	}
	if len(admin.AllowedModels) != 1 || admin.AllowedModels[0] != "*" {
		t.Errorf("admin AllowedModels = %v, want [*]", admin.AllowedModels)
	}
	if !admin.AllowPassthrough {
		t.Error("admin AllowPassthrough must be true")
	}

	auditor, ok := byName[auth.AuditorRole]
	if !ok {
		t.Fatalf("BuiltinRoles missing %q", auth.AuditorRole)
	}
	if len(auditor.AllowedModels) != 0 {
		t.Errorf("auditor AllowedModels = %v, want empty", auditor.AllowedModels)
	}
	if auditor.AllowPassthrough {
		t.Error("auditor AllowPassthrough must be false")
	}

	if len(roles) != 2 {
		t.Errorf("BuiltinRoles() has %d entries, want exactly 2 (demo roles stay dev-only)", len(roles))
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/seed/ -v`
Expected: compile error — `undefined: BuiltinRoles`.

- [ ] **Step 3: Implement**

Create `internal/seed/builtin.go`:

```go
package seed

import (
	"context"
	"fmt"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/auth"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/store"
)

// BuiltinRole is a role policy the gateway guarantees to exist.
type BuiltinRole struct {
	Role             string
	AllowedModels    []string
	AllowPassthrough bool
}

// BuiltinRoles returns the role policies required for the gateway to
// function: the admin role every bootstrap admin holds, and the auditor
// role. Demo roles (airllm_user) remain dev-seed-only.
func BuiltinRoles() []BuiltinRole {
	return []BuiltinRole{
		{Role: auth.AdminRole, AllowedModels: []string{"*"}, AllowPassthrough: true},
		{Role: auth.AuditorRole, AllowedModels: []string{}, AllowPassthrough: false},
	}
}

// EnsureBuiltinRoles inserts the built-in role policies if absent. It runs at
// every boot in every environment and never overwrites operator changes
// (ON CONFLICT DO NOTHING).
func EnsureBuiltinRoles(ctx context.Context, st *store.Store) error {
	for _, r := range BuiltinRoles() {
		if _, err := st.PG.Exec(ctx, `
			INSERT INTO roles_policy (role, allowed_models, allow_passthrough, limits)
			VALUES ($1, $2, $3, '{}'::jsonb)
			ON CONFLICT (role) DO NOTHING`,
			r.Role, r.AllowedModels, r.AllowPassthrough); err != nil {
			return fmt.Errorf("ensure builtin role %s: %w", r.Role, err)
		}
	}
	return nil
}
```

In `cmd/ipsupport-airllm/main.go`, immediately after the `st.Migrate` block (line 57-59):

```go
	if err := seed.EnsureBuiltinRoles(ctx, st); err != nil {
		return fmt.Errorf("ensure builtin roles: %w", err)
	}
```

(`seed` and `fmt` are already imported in main.go.)

- [ ] **Step 4: Run tests + build**

Run: `go test ./internal/seed/ -v && go build ./... && go test ./...`
Expected: all PASS, clean build.

- [ ] **Step 5: Commit**

```bash
gofmt -l . # must print nothing
git add internal/seed/ cmd/ipsupport-airllm/main.go
git commit -m "fix(auth): ensure builtin role policies exist at boot in every env"
```

---

### Task 4: Alias editor datalist fed by the catalog endpoint

**Files:**
- Modify: `web/static/app.js` — function `editAlias` (~line 886) and its `addRow` helper (~line 926)

**Interfaces:**
- Consumes: `GET /api/admin/providers/{name}/models` (Task 2); existing `api(method, path)` helper; existing `addRow(t)` / `provOpts(sel)` in `editAlias`; `esc()`.
- Produces: user-visible only — each target row's `upstream model` input gets datalist suggestions that refresh when the row's provider changes. No JS API surface.

- [ ] **Step 1: Implement (no JS test infra in this repo; verification is `node --check` + live walk)**

In `web/static/app.js`, inside `editAlias`, after the `provOpts` definition and before `function addRow(t)`, add:

```js
  // Upstream model suggestions: fetched per provider, memoized for the
  // lifetime of this modal. Failure or unsupported -> empty list; the input
  // stays free-text either way.
  const modelLists = {};
  let rowSeq = 0;
  async function loadModels(prov, dl) {
    if (!(prov in modelLists)) {
      try {
        const r = await api("GET", `/api/admin/providers/${encodeURIComponent(prov)}/models`);
        modelLists[prov] = (r.ok && r.data && r.data.models) || [];
      } catch { modelLists[prov] = []; }
    }
    dl.innerHTML = modelLists[prov].map((m) => `<option value="${esc(m)}">`).join("");
  }
```

Then modify `addRow(t)`: give the model input a per-row datalist and wire the
provider select. Replace the current `row.innerHTML` model-input line

```js
      <input class="t-model" placeholder="upstream model" value="${esc(t.upstream_model || "")}" style="flex:1;min-width:120px" />
```

with

```js
      <input class="t-model" list="al-models-${rowSeq}" placeholder="upstream model" value="${esc(t.upstream_model || "")}" style="flex:1;min-width:120px" />
      <datalist id="al-models-${rowSeq}"></datalist>
```

and at the end of `addRow` (after the existing `t-del` listener, before `tdiv.appendChild(row)`), add:

```js
    rowSeq++;
    const dl = row.querySelector("datalist");
    const provSel = row.querySelector(".t-prov");
    loadModels(provSel.value, dl);
    provSel.addEventListener("change", () => loadModels(provSel.value, dl));
```

Note: `rowSeq` must be incremented before the next `addRow` call reuses the id;
incrementing inside `addRow` after building `row.innerHTML` is correct because
the template already interpolated the current value.

- [ ] **Step 2: Syntax check**

Run: `node --check web/static/app.js`
Expected: no output (exit 0).

- [ ] **Step 3: Commit**

```bash
git add web/static/app.js
git commit -m "feat(ui): upstream model datalist in the alias editor"
```

---

### Task 5: Live verification (dev stack)

**Files:** none (verification only)

- [ ] **Step 1: Boot the dev stack**

Run: `make compose-up` then `make run` (or the repo's documented dev boot; `ENV=dev AUTH_MODE=local`). Wait for "dev mock seeded".

- [ ] **Step 2: Verify catalog endpoint end-to-end**

```bash
# login as dev admin (see seed.DevPassword) and call:
curl -s -b cookies.txt http://127.0.0.1:8080/api/admin/providers/mock/models
```

Expected: `{"models":["mock-gpt","mock-large","mock-small"]}`.

- [ ] **Step 3: Verify builtin roles on a prod-like boot**

```bash
# wipe the dev DB volume, boot once with ENV=prod AUTH_MODE=local, then:
psql "$DATABASE_URL" -c "SELECT role FROM roles_policy ORDER BY role"
```

Expected: `airllm_admin` and `airllm_auditor` rows exist (and only those).
Then: edit the bootstrap admin user via `PUT /api/admin/users/{id}` keeping
role `airllm_admin` → 200 (the prod bug is gone).

- [ ] **Step 4: UI walk**

Open the SPA → Admin → aliases → New alias: the `upstream model` input offers
`mock-gpt` / `mock-large` / `mock-small` for provider `mock`; switching
provider refreshes suggestions; manual free-text entry still works.
