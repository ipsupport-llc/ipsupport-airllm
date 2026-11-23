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
	if l.Locked(ctx, "anyone", "192.0.2.1") {
		t.Error("a nil *LoginLimiter must report not-locked")
	}
	l.RecordFailure(ctx, "anyone", "192.0.2.1") // must not panic
	l.RecordSuccess(ctx, "anyone", "192.0.2.1")

	l2 := NewLoginLimiter(nil)
	if l2.Locked(ctx, "anyone", "192.0.2.1") {
		t.Error("a LoginLimiter with a nil client must report not-locked")
	}
	l2.RecordFailure(ctx, "anyone", "192.0.2.1") // must not panic
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
	const ip = "192.0.2.1"
	t.Cleanup(func() { _ = rdb.Del(context.Background(), loginFailKey(username, ip)).Err() })

	if l.Locked(ctx, username, ip) {
		t.Fatal("a fresh username must not start locked")
	}

	for i := 0; i < loginMaxAttempts-1; i++ {
		l.RecordFailure(ctx, username, ip)
	}
	if l.Locked(ctx, username, ip) {
		t.Fatalf("must not be locked at %d failures (threshold is %d)", loginMaxAttempts-1, loginMaxAttempts)
	}

	l.RecordFailure(ctx, username, ip) // the Nth failure
	if !l.Locked(ctx, username, ip) {
		t.Fatalf("must be locked at %d failures", loginMaxAttempts)
	}

	l.RecordSuccess(ctx, username, ip)
	if l.Locked(ctx, username, ip) {
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
	const ip = "192.0.2.1"
	t.Cleanup(func() { _ = rdb.Del(context.Background(), loginFailKey(mixedCase, ip)).Err() })

	for i := 0; i < loginMaxAttempts; i++ {
		l.RecordFailure(ctx, mixedCase, ip)
	}
	if !l.Locked(ctx, lowerCase, ip) {
		t.Error("a different-case variant of the same username must see the same lockout")
	}
}

// TestLoginLimiterLockoutIsPerClientIP proves failures from one client IP
// don't lock the same username out for a different IP: anyone who knows a
// username must not be able to lock its owner out of the console.
func TestLoginLimiterLockoutIsPerClientIP(t *testing.T) {
	rdb := testLoginLimiterRedis(t)
	ctx := context.Background()
	l := NewLoginLimiter(rdb)
	username := fmt.Sprintf("ratelimit-ip-test-%d", time.Now().UnixNano())
	const attacker, owner = "203.0.113.7", "198.51.100.9"
	t.Cleanup(func() {
		_ = rdb.Del(context.Background(), loginFailKey(username, attacker), loginFailKey(username, owner)).Err()
	})

	for i := 0; i < loginMaxAttempts; i++ {
		l.RecordFailure(ctx, username, attacker)
	}
	if !l.Locked(ctx, username, attacker) {
		t.Fatal("the failing IP must be locked")
	}
	if l.Locked(ctx, username, owner) {
		t.Fatal("another IP must not be locked out by someone else's failures")
	}
}

func TestLoginFailKeyDistinctPerIP(t *testing.T) {
	if loginFailKey("Admin", "203.0.113.7") == loginFailKey("admin", "198.51.100.9") {
		t.Fatal("different client IPs must get different keys")
	}
	if loginFailKey("Admin", "2001:db8::1") != loginFailKey("admin", "2001:db8::1") {
		t.Fatal("the username part must be case-insensitive")
	}
}
