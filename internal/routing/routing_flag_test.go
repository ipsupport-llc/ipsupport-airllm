package routing

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/store"
)

// testPool connects to TEST_DATABASE_URL or skips. The DB must have the
// migrations applied (run the dev compose stack: docker compose -f
// deploy/docker-compose.yml up --build -d app).
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping dlp_model_scan flag test")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// TestDLPModelScanFlag exercises the dlp_model_scan column end to end.
//
// The alias branch of Router.Resolve queries through r.st.PG
// (*pgxpool.Pool), a concrete type a pgx.Tx cannot stand in for, so a
// fixture alias can't be pointed at from inside a rolled-back transaction
// via a real Router. That half of the test therefore runs the exact query
// Resolve issues (see internal/routing/routing.go) against the tx directly —
// honest about the schema/query semantics without needing a live Router, and
// it leaves no rows behind.
//
// The passthrough branch ("provider/upstream") never touches model_aliases
// and always sets DLPModelScan: true unconditionally in code, so it's
// exercised through the real Router.Resolve against the live pool (its one
// fixture row is deleted via defer since it can't ride the rolled-back tx).
func TestDLPModelScanFlag(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)

	// Skip with a clear message if the migration hasn't been applied yet.
	if _, err := tx.Exec(ctx, `SELECT dlp_model_scan FROM model_aliases LIMIT 0`); err != nil {
		t.Skipf("dlp_model_scan column not present (migration 0008 not applied?): %v", err)
	}

	mustExec := func(sql string, args ...any) {
		t.Helper()
		if _, err := tx.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %s: %v", sql, err)
		}
	}

	mustExec(`INSERT INTO providers (name, kind, base_url, enabled) VALUES ($1, $2, $3, $4)`,
		"flag-test-provider", "openai", "http://example.invalid", true)
	mustExec(`INSERT INTO model_aliases (alias, protocol, strategy, dlp_model_scan) VALUES ($1, $2, $3, $4)`,
		"flag-test-alias", "openai", "round_robin", false)
	mustExec(`INSERT INTO alias_targets (alias, priority, provider_name, upstream_model, upstream_protocol) VALUES ($1, $2, $3, $4, $5)`,
		"flag-test-alias", 0, "flag-test-provider", "upstream-model", "openai")

	// Same query as the alias branch of Router.Resolve.
	var strategy string
	var dlpModelScan bool
	if err := tx.QueryRow(ctx,
		`SELECT strategy, dlp_model_scan FROM model_aliases WHERE alias = $1`, "flag-test-alias",
	).Scan(&strategy, &dlpModelScan); err != nil {
		t.Fatalf("query alias: %v", err)
	}
	if dlpModelScan {
		t.Errorf("dlp_model_scan = true, want false for flag-test-alias")
	}

	// Passthrough branch: run the real Router.Resolve against the live pool.
	// The fixture row lives outside the rolled-back tx (Resolve queries the
	// pool), so give it a unique per-run name: an orphan from a killed test
	// process must never collide with (and break) future runs.
	passthroughProvider := fmt.Sprintf("flag-test-passthrough-%d", time.Now().UnixNano())
	if _, err := pool.Exec(ctx,
		`INSERT INTO providers (name, kind, base_url, enabled) VALUES ($1, $2, $3, $4)`,
		passthroughProvider, "openai", "http://example.invalid", true,
	); err != nil {
		t.Fatalf("insert passthrough provider: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(),
			`DELETE FROM providers WHERE name = $1`, passthroughProvider); err != nil {
			t.Errorf("cleanup passthrough provider: %v", err)
		}
	})

	r := NewRouter(&store.Store{PG: pool})
	plan, err := r.Resolve(ctx, passthroughProvider+"/upstream-model", true)
	if err != nil {
		t.Fatalf("Resolve passthrough: %v", err)
	}
	if !plan.DLPModelScan {
		t.Errorf("passthrough plan DLPModelScan = false, want true")
	}
}
