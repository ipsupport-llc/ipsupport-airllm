# API-Key Policy Snapshot Rebuild — Design

**Date:** 2026-07-08
**Status:** approved

## Problem

API keys carry a `policy_snapshot` frozen at issue time (fast one-lookup auth
path). The original design (2026-06-27 spec: "rebuild snapshots when a role
policy changes") planned automatic rebuilds — that half was never built. In
practice an operator edits a role and nothing changes for existing keys:
requests still 403, `/v1/models` stays empty, and the only workaround is
re-issuing the key. Three paths mutate effective roles today and none of them
rebuilds snapshots:

1. `PUT /api/admin/roles/{role}` — role policy edited (affects every user
   holding the role) — `internal/httpapi/api_admin.go:197`.
2. `PUT /api/admin/users/{id}` — user's role list changed —
   `internal/httpapi/api_users.go:94` → `PGUsers.Update`.
3. OIDC login — `PGUsers.UpsertOIDC` refreshes roles from IdP claims —
   `internal/store/users.go:49`.

## Decision

Complete the original design: rebuild the snapshots of affected users' active
keys **in the same transaction** as the role/user mutation. The auth hot path
is untouched (still one lookup). No manual "refresh" button.

## Design

### Merge logic moves to the store (single source of truth)

`effectivePolicyJSON` (currently `internal/httpapi/api_self.go:251`) moves to
`internal/store` unchanged in semantics — union of allowed models (sorted,
deduped), OR of passthrough, first non-empty limits:

```go
// internal/store/policy_snapshot.go
package store

// Querier is the subset of pgxpool.Pool / pgx.Tx the snapshot code needs.
type Querier interface {
    Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
    Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// EffectivePolicy merges the policies of roles into one snapshot JSON.
func EffectivePolicy(ctx context.Context, q Querier, roles []string) ([]byte, error)

// RebuildKeySnapshotsUser recomputes one user's snapshot and applies it to
// their active keys.
func RebuildKeySnapshotsUser(ctx context.Context, q Querier, userID string) error

// RebuildKeySnapshotsRole rebuilds snapshots for every user holding the role.
func RebuildKeySnapshotsRole(ctx context.Context, q Querier, role string) error
```

- `RebuildKeySnapshotsUser`: `SELECT roles FROM users WHERE id=$1` →
  `EffectivePolicy` → `UPDATE api_keys SET policy_snapshot=$2 WHERE
  user_id=$1 AND status='active'`.
- `RebuildKeySnapshotsRole`: `SELECT id::text FROM users WHERE $1 =
  ANY(roles)` → loop `RebuildKeySnapshotsUser`. Admin-frequency operation;
  N+1 inside one transaction is fine.
- `internal/httpapi` key creation (`api_self.go:77`) calls
  `store.EffectivePolicy` with `s.st.PG`; the old `effectivePolicyJSON`
  method is deleted.

### Call-site changes (each wraps mutation + rebuild in one pgx.Tx)

1. `handleAdminPutRole`: `tx := s.st.PG.Begin` → role upsert (existing SQL,
   via tx) → `store.RebuildKeySnapshotsRole(ctx, tx, role)` → commit.
2. `handleUpdateUser`: `PGUsers.Update` gains a `Querier` first-arg variant
   (or the handler passes a tx through a new `UpdateQ` method — implementer's
   choice, keep the store owning the SQL); handler wraps update + 
   `RebuildKeySnapshotsUser` in one tx.
3. `PGUsers.UpsertOIDC`: wraps its upsert + `RebuildKeySnapshotsUser` in one
   tx internally (runs at every OIDC login; one extra SELECT + one UPDATE of
   the user's keys — acceptable; no roles-changed diffing, unconditional
   rebuild keeps it simple).

Local-auth login does not touch roles — no change there.

### Failure semantics

Any rebuild error rolls back the whole mutation (role/user edit fails with
500) — the operator retries; there is never a window where the role says one
thing and active keys another.

### Out of scope

- No change to snapshot semantics at issue time, the auth path, or the
  `policy_snapshot` column.
- No "refresh" UI. No rebuild on role *deletion* (no such endpoint exists).
- Revoked keys are not rewritten.

## Testing

- Unit: no PG harness exists; the merge semantics move verbatim (they were
  exercised indirectly before and keep their behavior). New logic is
  SQL-glued, so:
- Integration (new, opt-in): `internal/store/policy_snapshot_test.go` gated
  on `TEST_DATABASE_URL` (`t.Skip` when unset — CI stays green). Against the
  compose Postgres: seed a user + role + active/revoked keys; edit role via
  `RebuildKeySnapshotsRole` → active key snapshot updated, revoked untouched;
  change user roles + `RebuildKeySnapshotsUser` → snapshot reflects merge
  rules (union, OR, first non-empty limits).
- Live (compose): create key under a restricted role → 403 on a model; edit
  the role to allow it → the SAME key immediately gets 200 and `/v1/models`
  lists the alias. No key re-issue.
