package store

import (
	"context"
	"testing"
	"time"
)

// TestMigrateSerializesConcurrentCallers proves Migrate's advisory lock
// actually serializes two concurrent callers (Store/config I3 fix) — a
// multi-replica rolling deploy can start several processes on the new
// image at once, all calling Migrate simultaneously. It holds the SAME
// advisory lock key on a separate connection (simulating another process
// already mid-migration), then confirms a concurrent Migrate call blocks
// until that lock releases.
func TestMigrateSerializesConcurrentCallers(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	st := &Store{PG: pool}

	heldConn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire held conn: %v", err)
	}
	defer heldConn.Release()
	if _, err := heldConn.Exec(ctx, `SELECT pg_advisory_lock($1)`, int64(migrationLockKey)); err != nil {
		t.Fatalf("acquire held lock: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- st.Migrate(ctx)
	}()

	select {
	case err := <-done:
		t.Fatalf("Migrate completed (err=%v) before the holding connection released the lock — not serialized", err)
	case <-time.After(150 * time.Millisecond):
	}

	if _, err := heldConn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, int64(migrationLockKey)); err != nil {
		t.Fatalf("release held lock: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Migrate returned an error after the lock released: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Migrate never completed after the lock was released")
	}
}
