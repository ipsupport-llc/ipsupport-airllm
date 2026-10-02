package limits

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/policy"
)

// testRedis connects to TEST_REDIS_URL or skips. Point it at the dev compose
// stack's Redis (make compose-up): redis://127.0.0.1:56379/0.
func testRedis(t *testing.T) *redis.Client {
	t.Helper()
	dsn := os.Getenv("TEST_REDIS_URL")
	if dsn == "" {
		t.Skip("TEST_REDIS_URL not set; skipping limiter integration test")
	}
	opt, err := redis.ParseURL(dsn)
	if err != nil {
		t.Fatalf("parse redis url: %v", err)
	}
	rdb := redis.NewClient(opt)
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

func testKey(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("limiter-test-%d", time.Now().UnixNano())
}

// TestCheckReservationIsVisibleAndCorrected proves the two halves of the
// Critical fix (racy check-then-increment) in a single-threaded sequence:
// (1) an allowed Check's reservation is immediately visible to the very next
// Check on the same key (closing the race window a sequential test can
// observe deterministically), and (2) Add's correction nets the reservation
// down to the real usage, freeing headroom a reservation alone would have
// kept locked up.
func TestCheckReservationIsVisibleAndCorrected(t *testing.T) {
	rdb := testRedis(t)
	ctx := context.Background()
	l := New(rdb)
	key := testKey(t)
	// A request is denied when EXISTING usage already reached the limit
	// (used >= limit) — the same comparison the unfixed code always used.
	// Limit == the first reservation so check 2 lands exactly at that
	// boundary, deterministically proving check 2 sees check 1's write.
	lim := policy.Limits{Tokens: map[string]int64{"5h": 1000}}

	// First request: reserve 1000 tokens. Allowed (0 used so far).
	dec1, err := l.Check(ctx, key, lim, 1000)
	if err != nil {
		t.Fatalf("check 1: %v", err)
	}
	if !dec1.Allowed || dec1.ReservedTokens != 1000 || dec1.ReservedBucket == 0 {
		t.Fatalf("check 1 = %+v, want allowed with a 1000-token reservation", dec1)
	}

	// Second request, same key: usage is now 1000 (check 1's reservation),
	// which already reached the 1000 limit. If this were allowed, check 1's
	// reservation wasn't actually visible yet.
	dec2, err := l.Check(ctx, key, lim, 1)
	if err != nil {
		t.Fatalf("check 2: %v", err)
	}
	if dec2.Allowed {
		t.Fatalf("check 2 should be denied (reservation from check 1 must already count), got %+v", dec2)
	}

	// The first request actually only used 200 tokens. Add corrects the
	// reservation down to the real usage.
	if err := l.Add(ctx, key, 200, 0, 0, 0, dec1.ReservedTokens, dec1.ReservedBucket); err != nil {
		t.Fatalf("add: %v", err)
	}

	// Now usage is 200 (corrected), well under the 1000 limit -> a new
	// reservation must be allowed again, proving the correction actually
	// freed the unused headroom instead of leaking it.
	dec3, err := l.Check(ctx, key, lim, 500)
	if err != nil {
		t.Fatalf("check 3: %v", err)
	}
	if !dec3.Allowed {
		t.Fatalf("check 3 should be allowed after the correction freed headroom, got %+v", dec3)
	}
}

// TestCheckSerializesConcurrentReservations proves the race itself is
// closed under real concurrency, not just in a controlled sequential
// script: N goroutines call Check simultaneously for the SAME key with a
// tight limit, and the number of Allowed decisions must be bounded exactly
// by limit/reservation — not by how many happened to read stale state
// before another's reservation landed.
func TestCheckSerializesConcurrentReservations(t *testing.T) {
	rdb := testRedis(t)
	ctx := context.Background()
	l := New(rdb)
	key := testKey(t)
	lim := policy.Limits{Tokens: map[string]int64{"5h": 500}}
	const (
		reservation = 100
		callers     = 10
		wantAllowed = 5 // 500 / 100
	)

	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed := 0
	errs := 0
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func() {
			defer wg.Done()
			dec, err := l.Check(ctx, key, lim, reservation)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs++
				return
			}
			if dec.Allowed {
				allowed++
			}
		}()
	}
	wg.Wait()

	if errs != 0 {
		t.Fatalf("%d Check calls returned an error", errs)
	}
	if allowed != wantAllowed {
		t.Fatalf("allowed = %d, want exactly %d (limit %d / reservation %d) — concurrent checks are not serialized",
			allowed, wantAllowed, 500, reservation)
	}
}
