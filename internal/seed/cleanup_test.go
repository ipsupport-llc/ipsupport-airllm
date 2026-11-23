package seed

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/apikey"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/auth"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/store"
)

// TestCleanupDevNeutralizesLeftoverDemoData proves a boot without SEED_DEV
// revokes the well-known demo key and disables demo accounts that still use
// the well-known password, leaves an account whose password was changed
// alone, logs a WARN per action, and is a no-op on a clean database and on
// a second run.
func TestCleanupDevNeutralizesLeftoverDemoData(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	st := &store.Store{PG: pool}
	keyHash := apikey.Hash(DevToken)

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	var demoRows int
	if err := pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM users WHERE subject = ANY($1)) +
		       (SELECT count(*) FROM api_keys WHERE hash = $2)`,
		devUserSubjects, keyHash).Scan(&demoRows); err != nil {
		t.Fatalf("count demo rows: %v", err)
	}
	if demoRows == 0 {
		if err := CleanupDev(ctx, st); err != nil {
			t.Fatalf("cleanup on a clean DB: %v", err)
		}
		if buf.Len() != 0 {
			t.Fatalf("cleanup on a clean DB must log nothing, got %q", buf.String())
		}
	} else {
		t.Log("database already has demo rows; skipping the clean-DB check")
	}

	// Snapshot what this test changes so a shared dev DB is restored.
	type userState struct {
		disabled bool
		hash     string
	}
	if _, err := Dev(ctx, st); err != nil {
		t.Fatalf("seed: %v", err)
	}
	orig := map[string]userState{}
	for _, sub := range devUserSubjects {
		var u userState
		if err := pool.QueryRow(ctx, `SELECT disabled, password_hash FROM users WHERE subject = $1`, sub).
			Scan(&u.disabled, &u.hash); err != nil {
			t.Fatalf("read %s: %v", sub, err)
		}
		orig[sub] = u
	}
	var origKeyStatus string
	if err := pool.QueryRow(ctx, `SELECT status FROM api_keys WHERE hash = $1`, keyHash).Scan(&origKeyStatus); err != nil {
		t.Fatalf("read key: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		for sub, u := range orig {
			_, _ = pool.Exec(bg, `UPDATE users SET disabled = $2, password_hash = $3 WHERE subject = $1`, sub, u.disabled, u.hash)
		}
		_, _ = pool.Exec(bg, `UPDATE api_keys SET status = $2 WHERE hash = $1`, keyHash, origKeyStatus)
	})

	// Start from: key active, all demo users enabled with the dev password,
	// except dev-auditor whose operator changed the password.
	devHash, err := auth.HashPassword(DevPassword)
	if err != nil {
		t.Fatal(err)
	}
	otherHash, err := auth.HashPassword("a-real-password-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE api_keys SET status = 'active' WHERE hash = $1`, keyHash); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET disabled = false, password_hash = $2 WHERE subject = ANY($1)`,
		devUserSubjects, devHash); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET password_hash = $1 WHERE subject = 'dev-auditor'`, otherHash); err != nil {
		t.Fatal(err)
	}

	buf.Reset()
	if err := CleanupDev(ctx, st); err != nil {
		t.Fatalf("cleanup: %v", err)
	}

	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM api_keys WHERE hash = $1`, keyHash).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "revoked" {
		t.Fatalf("demo key status = %q, want revoked", status)
	}
	want := map[string]bool{"dev-admin": true, "operator": true, "dev-auditor": false}
	for sub, wantDisabled := range want {
		var disabled bool
		if err := pool.QueryRow(ctx, `SELECT disabled FROM users WHERE subject = $1`, sub).Scan(&disabled); err != nil {
			t.Fatal(err)
		}
		if disabled != wantDisabled {
			t.Errorf("%s disabled = %v, want %v", sub, disabled, wantDisabled)
		}
	}
	logs := buf.String()
	if strings.Count(logs, "level=WARN") != 3 ||
		!strings.Contains(logs, "user=dev-admin") || !strings.Contains(logs, "user=operator") {
		t.Fatalf("expected 3 WARNs (key, dev-admin, operator), got:\n%s", logs)
	}

	// Idempotent: nothing left to do, nothing logged.
	buf.Reset()
	if err := CleanupDev(ctx, st); err != nil {
		t.Fatalf("second cleanup: %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("second cleanup must be a no-op, logged %q", buf.String())
	}
}
