package store

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
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

// countingQuerier wraps a Querier and counts QueryRow calls, to prove a
// rebuild path does (or doesn't) issue one extra query per user.
type countingQuerier struct {
	Querier
	queryRowCalls int
}

func (c *countingQuerier) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	c.queryRowCalls++
	return c.Querier.QueryRow(ctx, sql, args...)
}

// TestRebuildKeySnapshotsRoleAvoidsPerUserRoleQuery is the Store/config
// Minor fix: RebuildKeySnapshotsRole used to rebuild each affected user via
// RebuildKeySnapshotsUser, which re-queries that one user's roles even
// though the role path's own initial query already knows every affected
// user's id — an N+1 pattern (one extra "SELECT roles FROM users WHERE
// id=$1" per user). The only QueryRow call anywhere in this rebuild path is
// that one, so counting QueryRow calls proves whether it's gone.
func TestRebuildKeySnapshotsRoleAvoidsPerUserRoleQuery(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
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
		VALUES ('snaptest_n1', ARRAY['m1'], false, '{}'::jsonb)`)
	for i := range 3 {
		var uid string
		subject := fmt.Sprintf("snaptest-n1-user-%d", i)
		if err := tx.QueryRow(ctx, `INSERT INTO users (subject, email, display, roles)
			VALUES ($1, $1, $1, ARRAY['snaptest_n1']) RETURNING id::text`, subject).Scan(&uid); err != nil {
			t.Fatalf("insert user %d: %v", i, err)
		}
	}

	cq := &countingQuerier{Querier: tx}
	if err := RebuildKeySnapshotsRole(ctx, cq, "snaptest_n1"); err != nil {
		t.Fatalf("RebuildKeySnapshotsRole: %v", err)
	}
	if cq.queryRowCalls != 0 {
		t.Errorf("QueryRow calls = %d, want 0 (role rebuild must not re-query each user's roles individually)", cq.queryRowCalls)
	}
}

// TestEffectivePolicyMultiRoleLimitsAreDeterministic is the Store/config
// Minor fix: roles_policy has no precedence column, so when TWO roles both
// define non-empty (and different) limits, "first non-empty wins" used to
// depend on whatever order an unordered scan happened to return. Proves the
// merge is now deterministic — alphabetically first by role name — and,
// crucially, independent of the ORDER the caller's roles slice lists them
// in (only the database's own ORDER BY controls this, not Go-side order).
func TestEffectivePolicyMultiRoleLimitsAreDeterministic(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
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
	// Inserted in REVERSE alphabetical order on purpose: a plain sequential
	// scan (no ORDER BY) tends to return rows in physical/insertion order,
	// so this is what actually exercises the fix — if the fix regressed,
	// this table's own incidental row order would pick z's limits (999),
	// not a's (111).
	mustExec(`INSERT INTO roles_policy (role, allowed_models, allow_passthrough, limits)
		VALUES ('snaptest_ambig_z', ARRAY[]::text[], false, '{"tokens":{"24h":999}}'::jsonb)`)
	mustExec(`INSERT INTO roles_policy (role, allowed_models, allow_passthrough, limits)
		VALUES ('snaptest_ambig_a', ARRAY[]::text[], false, '{"tokens":{"24h":111}}'::jsonb)`)

	for _, roles := range [][]string{
		{"snaptest_ambig_a", "snaptest_ambig_z"},
		{"snaptest_ambig_z", "snaptest_ambig_a"}, // reversed slice order must not change the result
	} {
		raw, err := EffectivePolicy(ctx, tx, roles)
		if err != nil {
			t.Fatalf("EffectivePolicy(%v): %v", roles, err)
		}
		var eff map[string]json.RawMessage
		if err := json.Unmarshal(raw, &eff); err != nil {
			t.Fatal(err)
		}
		var limits map[string]map[string]int
		if err := json.Unmarshal(eff["limits"], &limits); err != nil {
			t.Fatal(err)
		}
		if got := limits["tokens"]["24h"]; got != 111 {
			t.Errorf("roles=%v: tokens.24h = %d, want 111 (snaptest_ambig_a, alphabetically first)", roles, got)
		}
	}
}
