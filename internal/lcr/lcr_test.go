package lcr

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock               { return &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)} }
func (c *fakeClock) Now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *fakeClock) Advance(d time.Duration) { c.mu.Lock(); c.now = c.now.Add(d); c.mu.Unlock() }

func testKey() Key { return Key{Provider: "test-provider", UpstreamModel: "test-model"} }

func TestSnapshotUnknownKeyDefaultsToOne(t *testing.T) {
	s := New(nil)
	snap := s.Snapshot(context.Background(), []Key{testKey()})
	if snap[testKey()] != 1.0 {
		t.Errorf("Snapshot of an unrecorded key = %v, want 1.0 (cold start)", snap[testKey()])
	}
}

func TestRecordThenSnapshotEWMA(t *testing.T) {
	dsn := requireRedis(t)
	opt, _ := redis.ParseURL(dsn)
	rdb := redis.NewClient(opt)
	t.Cleanup(func() { _ = rdb.Close() })

	clk := newFakeClock()
	s := New(rdb).WithClock(clk.Now)
	k := Key{Provider: "lcr-test", UpstreamModel: "m"}
	t.Cleanup(func() { _ = rdb.Del(context.Background(), k.redisKey()).Err() })

	s.Record(context.Background(), k, false) // one failure from cold start (1.0)
	waitForWrite(t, rdb, k)
	snap := s.Snapshot(context.Background(), []Key{k})
	// alpha=0.1: avail = 0.1*0 + 0.9*1.0 = 0.9
	if got := snap[k]; got < 0.89 || got > 0.91 {
		t.Errorf("after one failure from cold start, avail = %v, want ~0.9", got)
	}
}

func TestRecoveryTTLResetsToColdStart(t *testing.T) {
	dsn := requireRedis(t)
	opt, _ := redis.ParseURL(dsn)
	rdb := redis.NewClient(opt)
	t.Cleanup(func() { _ = rdb.Close() })

	k := Key{Provider: "lcr-ttl-test", UpstreamModel: "m"}
	t.Cleanup(func() { _ = rdb.Del(context.Background(), k.redisKey()).Err() })
	s := New(rdb)
	s.Record(context.Background(), k, false)
	waitForWrite(t, rdb, k)
	// Force-expire the key to simulate the TTL having elapsed, rather than
	// sleeping 15 real minutes.
	if err := rdb.PExpire(context.Background(), k.redisKey(), time.Millisecond).Err(); err != nil {
		t.Fatalf("force-expire: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	snap := s.Snapshot(context.Background(), []Key{k})
	if snap[k] != 1.0 {
		t.Errorf("after TTL expiry, avail = %v, want 1.0 (cold start again)", snap[k])
	}
}

func TestSnapshotFailsOpenWithoutRedis(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 50 * time.Millisecond, MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	s := New(rdb)
	start := time.Now()
	snap := s.Snapshot(context.Background(), []Key{testKey()})
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Snapshot with Redis down took %v, want it to fail fast", elapsed)
	}
	if snap[testKey()] != 1.0 {
		t.Errorf("Snapshot with Redis down = %v, want fail-open 1.0", snap[testKey()])
	}
}

func TestRecordDoesNotBlockCaller(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 2 * time.Second, MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	s := New(rdb)
	start := time.Now()
	s.Record(context.Background(), testKey(), true)
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Errorf("Record took %v before returning, want it to return immediately (write is fire-and-forget)", elapsed)
	}
}

func requireRedis(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("TEST_REDIS_URL")
	if dsn == "" {
		t.Skip("TEST_REDIS_URL not set; skipping real-Redis test")
	}
	return dsn
}

// waitForWrite polls briefly for Record's background write to land — Record
// is fire-and-forget by design, so the test can't assume it's visible the
// instant Record returns.
func waitForWrite(t *testing.T, rdb *redis.Client, k Key) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if n, _ := rdb.Exists(context.Background(), k.redisKey()).Result(); n == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("Record's background write never landed in Redis within 2s")
}
