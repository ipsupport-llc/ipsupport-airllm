# API-Key Policy Snapshot Rebuild Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Role/user-role edits (and OIDC logins) automatically rebuild the `policy_snapshot` of affected users' active API keys, in the same transaction as the mutation.

**Architecture:** The role-merge logic moves from `httpapi` to `internal/store` behind a `Querier` interface (pool or tx); three call sites wrap mutation + rebuild in one pgx transaction. Auth hot path untouched.

**Tech Stack:** Go 1.26, pgx v5 (already a dependency). No new deps.

**Spec:** `docs/superpowers/specs/2026-07-08-key-snapshot-rebuild-design.md`

## Global Constraints

- English only in the repo. No new Go dependencies.
- Merge semantics are UNCHANGED: union of allowed models (sorted, deduped), OR of passthrough, first non-empty limits (`{}` counts as empty).
- Rebuild touches ONLY `status = 'active'` keys.
- Mutation + rebuild = ONE transaction; rebuild failure rolls back the mutation.
- Auth hot path (`internal/httpapi/auth.go`) is not modified.
- Integration test gated on `TEST_DATABASE_URL` with `t.Skip` when unset (CI has no Postgres).
- Run `gofmt -l .` before every commit (must print nothing).

---

### Task 1: `internal/store/policy_snapshot.go` — Querier, EffectivePolicy, rebuild functions (+ integration test)

**Files:**
- Create: `internal/store/policy_snapshot.go`
- Test: `internal/store/policy_snapshot_test.go` (new, TEST_DATABASE_URL-gated)

**Interfaces:**
- Consumes: `policy.KeyPolicy` from `internal/policy` (fields `AllowedModels []string`, `AllowPassthrough bool`, `Limits json.RawMessage` — check the json tags in `internal/policy/policy.go` and keep the marshaled shape identical to what `effectivePolicyJSON` in `internal/httpapi/api_self.go:251` produces today).
- Produces (Task 2 depends on these exact signatures):
  - `type Querier interface { Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error); QueryRow(ctx context.Context, sql string, args ...any) pgx.Row; Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) }`
  - `func EffectivePolicy(ctx context.Context, q Querier, roles []string) ([]byte, error)`
  - `func RebuildKeySnapshotsUser(ctx context.Context, q Querier, userID string) error`
  - `func RebuildKeySnapshotsRole(ctx context.Context, q Querier, role string) error`

- [ ] **Step 1: Write the implementation** (integration test follows in Step 2 — the package cannot be unit-tested without PG, so red/green here is the gated integration test)

Create `internal/store/policy_snapshot.go`:

```go
package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/policy"
)

// Querier is the subset of *pgxpool.Pool and pgx.Tx the snapshot code needs,
// so mutation + rebuild can share one transaction.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// EffectivePolicy merges the policies of roles into one snapshot: union of
// allowed models (sorted, deduped), OR of passthrough, first non-empty
// limits. Moved verbatim from httpapi's effectivePolicyJSON so key issue and
// snapshot rebuild share one source of truth.
func EffectivePolicy(ctx context.Context, q Querier, roles []string) ([]byte, error) {
	eff := policy.KeyPolicy{}
	if len(roles) > 0 {
		rows, err := q.Query(ctx, `
			SELECT allowed_models, allow_passthrough, limits
			FROM roles_policy WHERE role = ANY($1)`, roles)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		modelSet := map[string]bool{}
		for rows.Next() {
			var models []string
			var passthrough bool
			var limits json.RawMessage
			if err := rows.Scan(&models, &passthrough, &limits); err != nil {
				return nil, err
			}
			for _, m := range models {
				modelSet[m] = true
			}
			eff.AllowPassthrough = eff.AllowPassthrough || passthrough
			if len(eff.Limits) == 0 && len(limits) > 0 && string(limits) != "{}" {
				eff.Limits = limits
			}
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		for m := range modelSet {
			eff.AllowedModels = append(eff.AllowedModels, m)
		}
		sort.Strings(eff.AllowedModels)
	}
	return json.Marshal(eff)
}

// RebuildKeySnapshotsUser recomputes one user's effective policy and applies
// it to their active keys. Revoked keys are left untouched.
func RebuildKeySnapshotsUser(ctx context.Context, q Querier, userID string) error {
	var roles []string
	if err := q.QueryRow(ctx,
		`SELECT roles FROM users WHERE id = $1`, userID).Scan(&roles); err != nil {
		return fmt.Errorf("load user roles: %w", err)
	}
	snap, err := EffectivePolicy(ctx, q, roles)
	if err != nil {
		return fmt.Errorf("effective policy: %w", err)
	}
	if _, err := q.Exec(ctx, `
		UPDATE api_keys SET policy_snapshot = $2
		WHERE user_id = $1 AND status = 'active'`, userID, snap); err != nil {
		return fmt.Errorf("update key snapshots: %w", err)
	}
	return nil
}

// RebuildKeySnapshotsRole rebuilds the key snapshots of every user holding
// the role. Runs at admin-edit frequency; the per-user loop is intentional
// (each user's snapshot merges their full role set).
func RebuildKeySnapshotsRole(ctx context.Context, q Querier, role string) error {
	rows, err := q.Query(ctx,
		`SELECT id::text FROM users WHERE $1 = ANY(roles)`, role)
	if err != nil {
		return fmt.Errorf("list users for role: %w", err)
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("scan user id: %w", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		if err := RebuildKeySnapshotsUser(ctx, q, id); err != nil {
			return err
		}
	}
	return nil
}
```

Note: collect ids first, THEN loop — pgx cannot run new queries while a rows
cursor is open on the same connection (a tx is one connection).

- [ ] **Step 2: Write the integration test**

Create `internal/store/policy_snapshot_test.go`:

```go
package store

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// testPool connects to TEST_DATABASE_URL or skips. The DB must have the
// migrations applied (run the dev compose stack: make compose-up).
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping store integration test")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestRebuildKeySnapshots(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	// Everything inside one rolled-back tx: the test leaves no rows behind.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)

	mustExec := func(sql string, args ...any) {
		t.Helper()
		if _, err := tx.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %s: %v", sql, err)
		}
	}

	mustExec(`INSERT INTO roles_policy (role, allowed_models, allow_passthrough, limits)
		VALUES ('snaptest_a', ARRAY['m1'], false, '{}'::jsonb)`)
	mustExec(`INSERT INTO roles_policy (role, allowed_models, allow_passthrough, limits)
		VALUES ('snaptest_b', ARRAY['m2'], true, '{"tokens":{"24h":1}}'::jsonb)`)

	var uid string
	if err := tx.QueryRow(ctx, `INSERT INTO users (subject, email, display, roles)
		VALUES ('snaptest-user', 'snap@test', 'snap', ARRAY['snaptest_a'])
		RETURNING id::text`).Scan(&uid); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	mustExec(`INSERT INTO api_keys (user_id, name, hash, prefix, last4, policy_snapshot, status)
		VALUES ($1, 'k-active', 'snaptest-hash-1', 'air_', '0001', '{}', 'active')`, uid)
	mustExec(`INSERT INTO api_keys (user_id, name, hash, prefix, last4, policy_snapshot, status)
		VALUES ($1, 'k-revoked', 'snaptest-hash-2', 'air_', '0002', '{}', 'revoked')`, uid)

	snapshotOf := func(name string) map[string]any {
		t.Helper()
		var raw []byte
		if err := tx.QueryRow(ctx,
			`SELECT policy_snapshot FROM api_keys WHERE user_id=$1 AND name=$2`,
			uid, name).Scan(&raw); err != nil {
			t.Fatalf("read snapshot %s: %v", name, err)
		}
		var m map[string]any
		json.Unmarshal(raw, &m)
		return m
	}

	// Role-edit path: rebuild by role applies the merged policy to active keys.
	if err := RebuildKeySnapshotsRole(ctx, tx, "snaptest_a"); err != nil {
		t.Fatalf("RebuildKeySnapshotsRole: %v", err)
	}
	got := snapshotOf("k-active")
	if am, _ := got["allowed_models"].([]any); len(am) != 1 || am[0] != "m1" {
		t.Errorf("active snapshot allowed_models = %v, want [m1]", got["allowed_models"])
	}
	if rev := snapshotOf("k-revoked"); len(rev) != 0 {
		t.Errorf("revoked key snapshot rewritten: %v (must stay {})", rev)
	}

	// User-roles-change path: add role b, rebuild by user — union + OR + limits.
	mustExec(`UPDATE users SET roles = ARRAY['snaptest_a','snaptest_b'] WHERE id = $1`, uid)
	if err := RebuildKeySnapshotsUser(ctx, tx, uid); err != nil {
		t.Fatalf("RebuildKeySnapshotsUser: %v", err)
	}
	got = snapshotOf("k-active")
	am, _ := got["allowed_models"].([]any)
	if len(am) != 2 || am[0] != "m1" || am[1] != "m2" {
		t.Errorf("merged allowed_models = %v, want [m1 m2]", am)
	}
	if got["allow_passthrough"] != true {
		t.Errorf("allow_passthrough = %v, want true (OR)", got["allow_passthrough"])
	}
	if got["limits"] == nil {
		t.Error("limits missing, want the non-empty role's limits")
	}
}
```

Check `internal/policy/policy.go` KeyPolicy json tags before finalizing the
assertions — the field names in the test (`allowed_models`,
`allow_passthrough`, `limits`) must match the actual tags.

- [ ] **Step 3: Run**

```bash
go build ./... && go vet ./internal/store/
go test ./internal/store/ -run RebuildKeySnapshots -v          # SKIP (no env)
TEST_DATABASE_URL="postgres://airllm:airllm@127.0.0.1:55432/airllm?sslmode=disable" \
  go test ./internal/store/ -run RebuildKeySnapshots -v        # PASS (compose stack up)
```
The compose stack (postgres on 127.0.0.1:55432) is already running on this
machine. If the second command fails to connect, report BLOCKED rather than
skipping the real run.

- [ ] **Step 4: Commit**

```bash
gofmt -l . # must print nothing
git add internal/store/
git commit -m "feat(store): effective-policy merge + key snapshot rebuild helpers"
```

---

### Task 2: Wire the three call sites (role edit, user edit, OIDC login) + drop the httpapi copy

**Files:**
- Modify: `internal/httpapi/api_admin.go:197-228` (`handleAdminPutRole` — tx + rebuild by role)
- Modify: `internal/httpapi/api_users.go:94-128` (`handleUpdateUser` — tx + rebuild by user)
- Modify: `internal/store/users.go:59-68` (`PGUsers.Update` gains a Querier param)
- Modify: `internal/store/users.go:49-57` (`UpsertOIDC` — tx + rebuild internally)
- Modify: `internal/httpapi/api_self.go:77` (use `store.EffectivePolicy`) and delete `effectivePolicyJSON` (api_self.go:248-286)

**Interfaces:**
- Consumes (from Task 1): `store.Querier`, `store.EffectivePolicy(ctx, q, roles)`, `store.RebuildKeySnapshotsUser(ctx, q, userID)`, `store.RebuildKeySnapshotsRole(ctx, q, role)`.
- Produces: `PGUsers.Update(ctx context.Context, q Querier, id, email, display string, roles []string, disabled bool) error` (signature change; `q == nil` is NOT allowed — pass `p.st.PG` where no tx is needed). Grep for other `users().Update(` call sites and update them.

- [ ] **Step 1: Rework `handleAdminPutRole`** (replace the single Exec at api_admin.go:213-225)

```go
	tx, err := s.st.PG.Begin(r.Context())
	if err != nil {
		writeControlError(w, http.StatusInternalServerError, "failed to save role")
		return
	}
	defer tx.Rollback(r.Context())
	if _, err := tx.Exec(r.Context(), `
		INSERT INTO roles_policy (role, allowed_models, allow_passthrough, limits)
		VALUES ($1, $2, $3, $4::jsonb)
		ON CONFLICT (role) DO UPDATE SET
			allowed_models = EXCLUDED.allowed_models,
			allow_passthrough = EXCLUDED.allow_passthrough,
			limits = EXCLUDED.limits,
			updated_at = now()`,
		role, body.AllowedModels, body.AllowPassthrough, string(body.Limits)); err != nil {
		writeControlError(w, http.StatusInternalServerError, "failed to save role")
		return
	}
	// Keep existing keys honest: re-snapshot every affected user's active
	// keys in the same transaction (the missing half of the original design).
	if err := store.RebuildKeySnapshotsRole(r.Context(), tx, role); err != nil {
		writeControlError(w, http.StatusInternalServerError, "failed to rebuild key snapshots")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeControlError(w, http.StatusInternalServerError, "failed to save role")
		return
	}
```

(`internal/store` is already imported in this file's package peers; add the import if missing. `pgx.Tx` satisfies `store.Querier`.)

- [ ] **Step 2: Rework `handleUpdateUser`** (replace the `s.users().Update(...)` call at api_users.go:122-125)

```go
	tx, err := s.st.PG.Begin(r.Context())
	if err != nil {
		writeControlError(w, http.StatusInternalServerError, "operation failed")
		return
	}
	defer tx.Rollback(r.Context())
	if err := s.users().Update(r.Context(), tx, id, body.Email, body.Display, body.Roles, body.Disabled); err != nil {
		s.writeUserErr(w, err)
		return
	}
	if err := store.RebuildKeySnapshotsUser(r.Context(), tx, id); err != nil {
		writeControlError(w, http.StatusInternalServerError, "failed to rebuild key snapshots")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeControlError(w, http.StatusInternalServerError, "operation failed")
		return
	}
```

- [ ] **Step 3: `PGUsers.Update` signature + `UpsertOIDC` tx** (internal/store/users.go)

```go
// Update modifies a user's profile/roles. q lets the caller run it inside a
// transaction (pass p.st.PG when no tx is needed).
func (p *PGUsers) Update(ctx context.Context, q Querier, id, email, display string, roles []string, disabled bool) error {
	tag, err := q.Exec(ctx, `
		UPDATE users SET email=$2, display=$3, roles=$4, disabled=$5, updated_at=now() WHERE id=$1`,
		id, email, display, roles, disabled)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrUserNotFound
	}
	return err
}

// UpsertOIDC creates/refreshes an OIDC user from IdP claims. Roles may change
// at any login, so the user's key snapshots are rebuilt in the same
// transaction.
func (p *PGUsers) UpsertOIDC(ctx context.Context, pr auth.Principal) (string, error) {
	tx, err := p.st.PG.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	var id string
	if err := tx.QueryRow(ctx, `
		INSERT INTO users (subject, email, display, roles, auth_source)
		VALUES ($1, $2, $1, $3, 'oidc')
		ON CONFLICT (subject) DO UPDATE SET email=EXCLUDED.email, roles=EXCLUDED.roles, auth_source='oidc', updated_at=now()
		RETURNING id::text`, pr.Subject, pr.Email, pr.Roles).Scan(&id); err != nil {
		return "", err
	}
	if err := RebuildKeySnapshotsUser(ctx, tx, id); err != nil {
		return "", err
	}
	return id, tx.Commit(ctx)
}
```

Grep for every other `.Update(` call on PGUsers (`grep -rn 'users().Update\|Users.Update' --include=*.go internal cmd`) and pass `p.st.PG`/`s.st.PG` as `q` where no transaction is involved. Check whether any auth-package interface (e.g. `auth.UserStore`) declares `Update` — if so, update the interface and its fakes to match.

- [ ] **Step 4: Key creation uses the store function; delete the httpapi copy**

In `internal/httpapi/api_self.go:77` replace:

```go
	snapshot, err := s.effectivePolicyJSON(r.Context(), sess.principal.Roles)
```
with
```go
	snapshot, err := store.EffectivePolicy(r.Context(), s.st.PG, sess.principal.Roles)
```

Delete the `effectivePolicyJSON` method (api_self.go:248-286) and now-unused imports (`sort`, possibly `policy` — run `go build` to find out). Add the `store` import if missing.

- [ ] **Step 5: Run everything**

```bash
gofmt -l .            # nothing
go build ./... && go vet ./...
go test ./...         # all green (integration test skips without env)
TEST_DATABASE_URL="postgres://airllm:airllm@127.0.0.1:55432/airllm?sslmode=disable" \
  go test ./internal/store/ -run RebuildKeySnapshots -v   # still PASS
```

- [ ] **Step 6: Commit**

```bash
git add internal/httpapi/ internal/store/
git commit -m "feat(auth): rebuild key policy snapshots on role/user edits and OIDC login"
```

---

### Task 3: Docs — snapshot lifecycle

**Files:**
- Modify: `docs/architecture.md:67` (the sentence saying policy is "snapshotted at issue time")
- Modify: `docs/configuration.md:131` (same claim in the roles section)

**Interfaces:** none (docs only).

- [ ] **Step 1: Update both sentences**

`docs/architecture.md:67` — replace the clause so it reads:

```markdown
- **Data-plane** uses API keys whose role policy is snapshotted onto the key
  and automatically re-snapshotted whenever an admin edits a role or a user's
  role list (and on OIDC login, where roles come from IdP claims) — one
  lookup on the hot path, never stale after a policy change.
```

`docs/configuration.md:131` — after the existing sentence about snapshotting
at issue time, append:

```markdown
Snapshots are rebuilt automatically — in the same transaction — when a role
policy or a user's role list changes, and on every OIDC login; existing keys
pick up policy edits immediately, no re-issue needed.
```

- [ ] **Step 2: Verify + commit**

```bash
make check-links   # docs link check still green
git add docs/architecture.md docs/configuration.md
git commit -m "docs: snapshot rebuild lifecycle"
```

---

### Task 4: Live verification (controller, compose stack)

**Files:** none (verification only).

- [ ] **Step 1:** Rebuild the app container from the branch (`docker compose -f deploy/docker-compose.yml up --build -d app`), wait for `/readyz`.
- [ ] **Step 2:** As dev-admin: create role `livetest` (allowed models `mock-gpt` only, no passthrough) via `PUT /api/admin/roles/livetest`; create a user with that role; login as that user; create a key.
- [ ] **Step 3:** With the key: `GET /v1/models` → only `mock-gpt`; `POST /v1/chat/completions` with `"model": "mock/mock-large"` (passthrough) → 403.
- [ ] **Step 4:** As dev-admin: edit the role — allowed models `*`, passthrough true. With the SAME key: `/v1/models` now lists all aliases; the passthrough call now succeeds. No key re-issue.
- [ ] **Step 5:** Edit the user's roles (drop `livetest`, give a restricted role) → the same key immediately loses access (403).
