package seed

import (
	"context"
	"testing"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/apikey"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/store"
)

// TestDevSeedDoesNotOverwriteOperatorEdits proves a re-seed (every app boot
// with ENV=dev+AUTH_MODE=local calls Dev unconditionally) does not clobber
// an operator's later changes to the seeded pricing row or the seeded API
// key's status (Store/config I4 fix) — both used to be a DO UPDATE that ran
// on every boot, silently reverting a revoked dev key back to active or a
// pricing tweak back to the hardcoded demo value.
func TestDevSeedDoesNotOverwriteOperatorEdits(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	st := &store.Store{PG: pool}

	// Establish the baseline (idempotent against whatever's already seeded).
	if _, err := Dev(ctx, st); err != nil {
		t.Fatalf("initial seed: %v", err)
	}

	k := apikey.Describe(DevToken)

	// Capture the current values so the dev environment can be restored
	// regardless of test outcome — this DB may be shared with a running
	// dev compose stack.
	var origInput float64
	if err := pool.QueryRow(ctx,
		`SELECT input_per_1m FROM pricing WHERE provider = '' AND model = 'mock-model-1'`,
	).Scan(&origInput); err != nil {
		t.Fatalf("read original pricing: %v", err)
	}
	var origStatus string
	if err := pool.QueryRow(ctx,
		`SELECT status FROM api_keys WHERE hash = $1`, k.Hash,
	).Scan(&origStatus); err != nil {
		t.Fatalf("read original key status: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, `UPDATE pricing SET input_per_1m = $1 WHERE provider = '' AND model = 'mock-model-1'`, origInput)
		_, _ = pool.Exec(bg, `UPDATE api_keys SET status = $1 WHERE hash = $2`, origStatus, k.Hash)
	})

	// Simulate operator edits: a pricing tweak and revoking the dev key.
	if _, err := pool.Exec(ctx,
		`UPDATE pricing SET input_per_1m = 999.99 WHERE provider = '' AND model = 'mock-model-1'`,
	); err != nil {
		t.Fatalf("simulate pricing edit: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE api_keys SET status = 'revoked' WHERE hash = $1`, k.Hash,
	); err != nil {
		t.Fatalf("simulate key revoke: %v", err)
	}

	// Re-seed, simulating the next app restart.
	if _, err := Dev(ctx, st); err != nil {
		t.Fatalf("re-seed: %v", err)
	}

	var gotInput float64
	if err := pool.QueryRow(ctx,
		`SELECT input_per_1m FROM pricing WHERE provider = '' AND model = 'mock-model-1'`,
	).Scan(&gotInput); err != nil {
		t.Fatalf("read pricing after re-seed: %v", err)
	}
	if gotInput != 999.99 {
		t.Errorf("pricing input_per_1m = %v after re-seed, want 999.99 (operator edit must survive)", gotInput)
	}

	var gotStatus string
	if err := pool.QueryRow(ctx,
		`SELECT status FROM api_keys WHERE hash = $1`, k.Hash,
	).Scan(&gotStatus); err != nil {
		t.Fatalf("read key status after re-seed: %v", err)
	}
	if gotStatus != "revoked" {
		t.Errorf("api key status = %q after re-seed, want \"revoked\" (operator revoke must survive)", gotStatus)
	}
}
