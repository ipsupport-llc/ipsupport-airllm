package unavail

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// fakeClock lets tests advance time deterministically instead of sleeping.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func testKey() Key { return Key{Provider: "muse api k", UpstreamModel: "muse-spark-1.3"} }

// TestCheckUnmarkedTargetDoesNotSkip proves the default state: a target
// never marked is never skipped.
func TestCheckUnmarkedTargetDoesNotSkip(t *testing.T) {
	s := New(nil)
	if _, skip := s.Check(context.Background(), testKey()); skip {
		t.Error("an unmarked target must not be skipped")
	}
}

// TestMarkThenCheckSkipsUntilExpiry is the core mechanic: marked means
// skipped until the duration elapses, then not.
func TestMarkThenCheckSkipsUntilExpiry(t *testing.T) {
	clk := newFakeClock()
	s := New(nil).WithClock(clk.Now)
	ctx := context.Background()
	k := testKey()

	dur := s.Mark(ctx, k, nil, 200*time.Millisecond, 0)
	if dur != 200*time.Millisecond {
		t.Fatalf("Mark returned %v, want 200ms (initial, no prior mark)", dur)
	}
	if remaining, skip := s.Check(ctx, k); !skip || remaining <= 0 {
		t.Fatalf("Check right after Mark: skip=%v remaining=%v, want skip=true with positive remaining", skip, remaining)
	}

	clk.Advance(199 * time.Millisecond)
	if _, skip := s.Check(ctx, k); !skip {
		t.Error("Check 1ms before expiry must still skip")
	}

	clk.Advance(2 * time.Millisecond)
	if _, skip := s.Check(ctx, k); skip {
		t.Error("Check after the mark's duration elapsed must not skip")
	}
}

// TestMarkHonorsRetryAfterExactly is the Retry-After plumbing this whole
// feature exists for: when the upstream gives an explicit value, use it
// exactly, not the gateway's own backoff math.
func TestMarkHonorsRetryAfterExactly(t *testing.T) {
	s := New(nil).WithClock(newFakeClock().Now)
	ra := 60 * time.Second
	dur := s.Mark(context.Background(), testKey(), &ra, 200*time.Millisecond, 8*time.Second)
	if dur != 60*time.Second {
		t.Errorf("Mark with Retry-After=60s returned %v, want exactly 60s (even though it exceeds the configured max)", dur)
	}
}

// TestMarkDoublesWithoutRetryAfterCappedAtMax covers the fallback schedule
// when the upstream sends no Retry-After: double from the last mark, capped.
func TestMarkDoublesWithoutRetryAfterCappedAtMax(t *testing.T) {
	s := New(nil).WithClock(newFakeClock().Now)
	ctx, k := context.Background(), testKey()
	const initial = 200 * time.Millisecond
	const max = 1 * time.Second

	want := []time.Duration{200 * time.Millisecond, 400 * time.Millisecond, 800 * time.Millisecond, 1 * time.Second, 1 * time.Second}
	for i, w := range want {
		got := s.Mark(ctx, k, nil, initial, max)
		if got != w {
			t.Errorf("mark #%d = %v, want %v", i+1, got, w)
		}
	}
}

// TestClearResetsBackoffMemory proves Clear is a real reset, not just an
// early unmark: the NEXT failure after Clear starts at initial again,
// instead of doubling from wherever the backoff had climbed to.
func TestClearResetsBackoffMemory(t *testing.T) {
	s := New(nil).WithClock(newFakeClock().Now)
	ctx, k := context.Background(), testKey()
	const initial = 200 * time.Millisecond

	s.Mark(ctx, k, nil, initial, 0)
	if got := s.Mark(ctx, k, nil, initial, 0); got != 400*time.Millisecond {
		t.Fatalf("second mark (pre-clear) = %v, want 400ms (doubled)", got)
	}
	s.Clear(ctx, k)
	if got := s.Mark(ctx, k, nil, initial, 0); got != initial {
		t.Errorf("mark after Clear = %v, want %v (fresh start, not doubled from pre-clear history)", got, initial)
	}
}

// TestMarkAndCheckRoundTripThroughRealRedis proves the JSON encoding and
// Redis GET/SET/EX plumbing actually work end to end, not just the local
// in-memory fallback every other test above exercises — and that the key
// lives under the gateway's own "air:" keyspace (see PR #157's breaker fix
// for why that convention matters here).
func TestMarkAndCheckRoundTripThroughRealRedis(t *testing.T) {
	dsn := os.Getenv("TEST_REDIS_URL")
	if dsn == "" {
		t.Skip("TEST_REDIS_URL not set; skipping real-Redis round-trip test")
	}
	opt, err := redis.ParseURL(dsn)
	if err != nil {
		t.Fatalf("parse redis url: %v", err)
	}
	rdb := redis.NewClient(opt)
	t.Cleanup(func() { _ = rdb.Close() })

	k := Key{Provider: fmt.Sprintf("unavail-test-%d", time.Now().UnixNano()), UpstreamModel: "m"}
	t.Cleanup(func() { _ = rdb.Del(context.Background(), k.redisKey()).Err() })
	if !strings.HasPrefix(k.redisKey(), "air:") {
		t.Fatalf("redis key %q must live under the air: keyspace", k.redisKey())
	}

	s := New(rdb)
	ctx := context.Background()
	if dur := s.Mark(ctx, k, nil, 5*time.Second, 0); dur != 5*time.Second {
		t.Fatalf("Mark = %v, want 5s", dur)
	}

	// A second Store instance (simulating a different replica) must see the
	// SAME mark through Redis, not just this process's own local map.
	s2 := New(rdb)
	if remaining, skip := s2.Check(ctx, k); !skip || remaining <= 0 {
		t.Errorf("a second Store instance: skip=%v remaining=%v, want skip=true via shared Redis state", skip, remaining)
	}
}

// TestStoreFailsOpenWithoutRedis proves the store never blocks or panics
// when Redis is unreachable, and that the in-process local fallback still
// tracks marks correctly on its own within the same replica.
func TestStoreFailsOpenWithoutRedis(t *testing.T) {
	// Nothing listens on this address: every Redis call fails fast.
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 50 * time.Millisecond, MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })

	clk := newFakeClock()
	s := New(rdb).WithClock(clk.Now)
	ctx, k := context.Background(), testKey()

	if _, skip := s.Check(ctx, k); skip {
		t.Fatal("an unmarked key with Redis down must not be skipped (fail open)")
	}

	start := time.Now()
	dur := s.Mark(ctx, k, nil, 200*time.Millisecond, 0)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Mark with Redis down took %v, want it to fail fast and fall back locally", elapsed)
	}
	if dur != 200*time.Millisecond {
		t.Fatalf("Mark with Redis down returned %v, want 200ms from the local fallback", dur)
	}
	if remaining, skip := s.Check(ctx, k); !skip || remaining <= 0 {
		t.Errorf("Check right after Mark with Redis down: skip=%v remaining=%v, want the local fallback to report skip=true", skip, remaining)
	}
}
