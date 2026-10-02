package ledger

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/store"
)

// TestRecordDropsWhenQueueFullWithoutBlocking is the DLP-I2 fix: Record must
// never block the request path on a database round trip. Workers are never
// started, so nothing ever drains the queue; overflowing it still returns
// promptly (the whole loop finishes well under a blocking send's timeout)
// and increments Dropped() instead of hanging.
func TestRecordDropsWhenQueueFullWithoutBlocking(t *testing.T) {
	l := New(nil) // workers never run in this test, so l.st is never touched
	start := time.Now()
	done := make(chan struct{})
	go func() {
		for i := 0; i < chanSize+50; i++ {
			l.Record(context.Background(), Entry{ProviderName: "test"})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Record blocked on a full queue instead of dropping")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Record took %v for %d calls against an undrained queue; want near-instant", elapsed, chanSize+50)
	}
	if l.Dropped() == 0 {
		t.Error("expected a non-zero dropped counter after overflowing the queue")
	}
}

// TestRecordAfterStopIsSafeNoOp mirrors capture.Pipeline's equivalent test:
// Record after Stop must not panic with "send on closed channel", and must
// not count as a drop (it returns before reaching the channel at all).
func TestRecordAfterStopIsSafeNoOp(t *testing.T) {
	l := New(nil)
	l.Start()
	l.Stop()
	l.Record(context.Background(), Entry{ProviderName: "test"}) // must not panic
	if l.Dropped() != 0 {
		t.Error("post-stop Record must not count as a drop (early return)")
	}
}

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping ledger integration test")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// TestStopDrainsBufferedEntries proves the actual async pipeline end to end:
// a Record call returns before the row exists, and Stop blocks until every
// buffered entry has actually been written — so a graceful shutdown never
// silently loses a request's usage row.
func TestStopDrainsBufferedEntries(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	l := New(&store.Store{PG: pool})
	l.Start()

	const provider = "ledger-test-stop-drain"
	t.Cleanup(func() {
		pool.Exec(context.Background(), `DELETE FROM usage_ledger WHERE provider_name = $1`, provider)
	})

	for i := 0; i < 5; i++ {
		l.Record(ctx, Entry{ProviderName: provider, Alias: "test", Status: 200})
	}
	l.Stop()

	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM usage_ledger WHERE provider_name = $1`, provider).Scan(&n); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if n != 5 {
		t.Errorf("rows persisted after Stop = %d, want 5 (Stop must drain the queue before returning)", n)
	}
}
