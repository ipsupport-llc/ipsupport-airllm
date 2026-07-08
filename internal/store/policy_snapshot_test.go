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
