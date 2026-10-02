package httpapi

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/apikey"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/store"
)

// TestLookupKeyRejectsDisabledUser proves a disabled user's otherwise-active
// API key stops authenticating immediately — the bug this guards against:
// lookupKey used to only check api_keys.status, never joining users.disabled,
// so a disabled account's keys kept working on the data plane indefinitely.
func TestLookupKeyRejectsDisabledUser(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	s := &Server{st: &store.Store{PG: pool}}

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	token := "air_test_" + suffix

	var userID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO users (subject, disabled) VALUES ($1, true) RETURNING id::text`,
		"disabled-key-test-"+suffix,
	).Scan(&userID); err != nil {
		t.Fatalf("insert disabled user: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, userID); err != nil {
			t.Errorf("cleanup user: %v", err)
		}
	})
	if _, err := pool.Exec(ctx, `
		INSERT INTO api_keys (user_id, hash, prefix, last4, status)
		VALUES ($1, $2, 'air_test', '0000', 'active')`,
		userID, apikey.Hash(token),
	); err != nil {
		t.Fatalf("insert api key: %v", err)
	}

	if _, err := s.lookupKey(ctx, token); err == nil {
		t.Fatal("lookupKey must reject a key whose owning user is disabled")
	}

	if _, err := pool.Exec(ctx, `UPDATE users SET disabled = false WHERE id = $1`, userID); err != nil {
		t.Fatalf("re-enable user: %v", err)
	}
	if _, err := s.lookupKey(ctx, token); err != nil {
		t.Fatalf("lookupKey must succeed once the owning user is re-enabled, got: %v", err)
	}
}
