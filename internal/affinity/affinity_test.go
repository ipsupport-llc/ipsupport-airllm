package affinity

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// testRedis connects to TEST_REDIS_URL or skips.
func testRedis(t *testing.T) *redis.Client {
	t.Helper()
	dsn := os.Getenv("TEST_REDIS_URL")
	if dsn == "" {
		t.Skip("TEST_REDIS_URL not set; skipping shared session pin test")
	}
	opt, err := redis.ParseURL(dsn)
	if err != nil {
		t.Fatalf("parse redis url: %v", err)
	}
	rdb := redis.NewClient(opt)
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

// A pin only moves forward, whichever store holds it — a later replica that
// never saw the session reads the later tier back from Redis — until it is
// forgotten.
func TestPinNeverMovesBack(t *testing.T) {
	ctx := context.Background()
	stores := map[string]func(t *testing.T) (writer, reader *Store){
		"local": func(t *testing.T) (*Store, *Store) { s := New(nil); return s, s },
		"redis": func(t *testing.T) (*Store, *Store) {
			rdb := testRedis(t)
			return New(rdb), New(rdb)
		},
	}
	for name, mk := range stores {
		t.Run(name, func(t *testing.T) {
			w, r := mk(t)
			alias := fmt.Sprintf("pin-%d", time.Now().UnixNano())
			if !w.Pin(ctx, alias, "call-1", 2, time.Minute) {
				t.Error("first pin did not report a move")
			}
			if w.Pin(ctx, alias, "call-1", 1, time.Minute) {
				t.Error("pinning an earlier tier reported a move")
			}
			if tier, ok := r.Pinned(ctx, alias, "call-1"); !ok || tier != 2 {
				t.Errorf("pinned = %d (%v), want 2", tier, ok)
			}
			if tier, ok := r.Pinned(ctx, alias, "call-2"); ok {
				t.Errorf("an unpinned session reads tier %d", tier)
			}
			if !w.Pin(ctx, alias, "call-1", 3, time.Minute) {
				t.Error("pinning a later tier did not report a move")
			}
			w.Forget(ctx, alias, "call-1")
			if !w.Pin(ctx, alias, "call-1", 1, time.Minute) {
				t.Error("pinning after Forget did not report a move")
			}
			if tier, ok := r.Pinned(ctx, alias, "call-1"); !ok || tier != 1 {
				t.Errorf("pinned after Forget = %d (%v), want 1", tier, ok)
			}
		})
	}
}

func TestRedisPinExpires(t *testing.T) {
	rdb := testRedis(t)
	ctx := context.Background()
	alias := fmt.Sprintf("pin-ttl-%d", time.Now().UnixNano())
	New(rdb).Pin(ctx, alias, "call-1", 1, 50*time.Millisecond)
	time.Sleep(100 * time.Millisecond)
	if tier, ok := New(rdb).Pinned(ctx, alias, "call-1"); ok {
		t.Errorf("pin still reads tier %d after its TTL", tier)
	}
}
