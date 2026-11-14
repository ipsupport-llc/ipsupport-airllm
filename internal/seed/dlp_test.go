package seed

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

func TestEnsureDLPDefaults(t *testing.T) {
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
	dlpValue := func() (json.RawMessage, bool) {
		t.Helper()
		var raw json.RawMessage
		if err := tx.QueryRow(ctx, `SELECT value FROM settings WHERE name = 'dlp'`).Scan(&raw); err != nil {
			return nil, false
		}
		return raw, true
	}

	// Clean slate: whatever the migrations/prior app boots left behind on
	// this connection is irrelevant — clear it for this tx.
	mustExec(`DELETE FROM settings WHERE name = 'dlp'`)

	// Empty modelURL and maxConcurrency<=0 (env unset) is a no-op: no row created.
	if err := EnsureDLPDefaults(ctx, tx, "", 0); err != nil {
		t.Fatalf("EnsureDLPDefaults(empty): %v", err)
	}
	if _, ok := dlpValue(); ok {
		t.Error("EnsureDLPDefaults(empty) created a row, want none")
	}

	// First boot: seeds the partial {"model_url": ...} row.
	if err := EnsureDLPDefaults(ctx, tx, "http://svc:8000", 0); err != nil {
		t.Fatalf("EnsureDLPDefaults(seed): %v", err)
	}
	raw, ok := dlpValue()
	if !ok {
		t.Fatal("EnsureDLPDefaults(seed) did not create a row")
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["model_url"] != "http://svc:8000" || got["model_enabled"] != true || len(got) != 2 {
		t.Errorf("seeded value = %v, want model_url + model_enabled:true only", got)
	}

	// Second boot with a different URL: existing row is never touched.
	if err := EnsureDLPDefaults(ctx, tx, "http://other:9000", 0); err != nil {
		t.Fatalf("EnsureDLPDefaults(reboot): %v", err)
	}
	raw, ok = dlpValue()
	if !ok {
		t.Fatal("dlp row disappeared after second call")
	}
	got = nil
	json.Unmarshal(raw, &got)
	if got["model_url"] != "http://svc:8000" {
		t.Errorf("model_url after reboot = %q, want unchanged %q", got["model_url"], "http://svc:8000")
	}

	// maxConcurrency > 0 is seeded alongside model_url on first boot.
	mustExec(`DELETE FROM settings WHERE name = 'dlp'`)
	if err := EnsureDLPDefaults(ctx, tx, "http://svc:8000", 4); err != nil {
		t.Fatalf("EnsureDLPDefaults(seed with concurrency): %v", err)
	}
	raw, ok = dlpValue()
	if !ok {
		t.Fatal("EnsureDLPDefaults(seed with concurrency) did not create a row")
	}
	got = nil
	json.Unmarshal(raw, &got)
	if mc, ok := got["model_max_concurrency"].(float64); !ok || mc != 4 {
		t.Errorf("seeded model_max_concurrency = %v, want 4", got["model_max_concurrency"])
	}
	if got["model_enabled"] != true {
		t.Errorf("seeded model_enabled = %v, want true", got["model_enabled"])
	}

	// An operator save (full config, unrelated to the seed shape) must also
	// never be clobbered by a later boot.
	mustExec(`DELETE FROM settings WHERE name = 'dlp'`)
	mustExec(`INSERT INTO settings (name, value) VALUES ('dlp', $1)`,
		[]byte(`{"enabled": false, "model_url": "http://operator-set:1234"}`))
	if err := EnsureDLPDefaults(ctx, tx, "http://svc:8000", 4); err != nil {
		t.Fatalf("EnsureDLPDefaults(operator-set): %v", err)
	}
	raw, ok = dlpValue()
	if !ok {
		t.Fatal("operator-set dlp row disappeared")
	}
	got = nil
	json.Unmarshal(raw, &got)
	if got["model_url"] != "http://operator-set:1234" {
		t.Errorf("operator-set model_url = %q, want untouched %q", got["model_url"], "http://operator-set:1234")
	}
	if got["enabled"] != false {
		t.Errorf("operator-set enabled = %v, want untouched false", got["enabled"])
	}
}
