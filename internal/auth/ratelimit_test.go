package auth

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// TestLoginLimiterNilIsSafe proves a nil *LoginLimiter (or one with a nil
// client) always reports "not locked" and never panics — the zero value
// must be safe to use, matching this codebase's fail-open posture for
// optional dependencies.
func TestLoginLimiterNilIsSafe(t *testing.T) {
	var l *LoginLimiter
	ctx := context.Background()
	if l.Locked(ctx, "anyone") {
		t.Error("a nil *LoginLimiter must report not-locked")
	}
	l.RecordFailure(ctx, "anyone") // must not panic
	l.RecordSuccess(ctx, "anyone")

	l2 := NewLoginLimiter(nil)
	if l2.Locked(ctx, "anyone") {
		t.Error("a LoginLimiter with a nil client must report not-locked")
	}
	l2.RecordFailure(ctx, "anyone") // must not panic
}

// testLoginLimiterRedis connects to TEST_REDIS_URL or skips.
func testLoginLimiterRedis(t *testing.T) *redis.Client {
	t.Helper()
	dsn := os.Getenv("TEST_REDIS_URL")
	if dsn == "" {
		t.Skip("TEST_REDIS_URL not set; skipping login-limiter integration test")
	}
	opt, err := redis.ParseURL(dsn)
	if err != nil {
		t.Fatalf("parse redis url: %v", err)
	}
	rdb := redis.NewClient(opt)
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

// TestLoginLimiterLocksAfterThresholdAndClearsOnSuccess proves the real
// Redis-backed lockout mechanism (Auth I2 fix): a username stays unlocked
// below the failure threshold, becomes locked at it, and a recorded success
// clears the lockout immediately rather than making the account wait out
// the window after it just proved it owns the password.
func TestLoginLimiterLocksAfterThresholdAndClearsOnSuccess(t *testing.T) {
	rdb := testLoginLimiterRedis(t)
	ctx := context.Background()
	l := NewLoginLimiter(rdb)
	username := fmt.Sprintf("ratelimit-test-%d", time.Now().UnixNano())
	t.Cleanup(func() { _ = rdb.Del(context.Background(), loginFailKey(username)).Err() })

	if l.Locked(ctx, username) {
		t.Fatal("a fresh username must not start locked")
	}

	for i := 0; i < loginMaxAttempts-1; i++ {
		l.RecordFailure(ctx, username)
	}
	if l.Locked(ctx, username) {
		t.Fatalf("must not be locked at %d failures (threshold is %d)", loginMaxAttempts-1, loginMaxAttempts)
	}

	l.RecordFailure(ctx, username) // the Nth failure
	if !l.Locked(ctx, username) {
		t.Fatalf("must be locked at %d failures", loginMaxAttempts)
	}

	l.RecordSuccess(ctx, username)
	if l.Locked(ctx, username) {
		t.Error("a recorded success must clear the lockout immediately")
	}
}

// TestLoginLimiterUsernameIsCaseInsensitive proves the lockout key matches
// ByUsername's own case-insensitive lookup, so varying the case of a login
// attempt can't be used to dodge the counter.
func TestLoginLimiterUsernameIsCaseInsensitive(t *testing.T) {
	rdb := testLoginLimiterRedis(t)
	ctx := context.Background()
	l := NewLoginLimiter(rdb)
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	mixedCase := "CaseTest-" + suffix
	lowerCase := "casetest-" + suffix
	t.Cleanup(func() { _ = rdb.Del(context.Background(), loginFailKey(mixedCase)).Err() })

	for i := 0; i < loginMaxAttempts; i++ {
		l.RecordFailure(ctx, mixedCase)
	}
	if !l.Locked(ctx, lowerCase) {
		t.Error("a different-case variant of the same username must see the same lockout")
	}
}
