package auth

import (
	"context"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// loginMaxAttempts is how many consecutive failed logins a username may have
// before LoginLimiter locks it out; loginLockout is how long the lockout
// lasts once triggered.
const (
	loginMaxAttempts = 10
	loginLockout     = 15 * time.Minute
)

// LoginLimiter throttles repeated failed login attempts per username, so a
// brute-force guesser can't try passwords without bound. Backed by Redis
// (not in-process state) because this gateway runs multiple replicas behind
// a load balancer; an in-memory counter would reset per pod and never
// actually bound a distributed attempt.
//
// A nil *LoginLimiter (or one built with a nil client) is always "not
// locked" and records nothing — the zero value is safe to use, matching the
// rest of this codebase's fail-open posture for optional dependencies.
type LoginLimiter struct {
	rdb *redis.Client
}

// NewLoginLimiter builds a LoginLimiter backed by rdb.
func NewLoginLimiter(rdb *redis.Client) *LoginLimiter {
	return &LoginLimiter{rdb: rdb}
}

func loginFailKey(username string) string {
	return "air:loginfail:" + strings.ToLower(username)
}

// Locked reports whether username is currently locked out from too many
// recent failed attempts. Fails open (not locked) on a Redis error — a
// Redis outage must not be able to lock every operator out of the console.
func (l *LoginLimiter) Locked(ctx context.Context, username string) bool {
	if l == nil || l.rdb == nil {
		return false
	}
	n, err := l.rdb.Get(ctx, loginFailKey(username)).Int()
	if err != nil {
		return false // covers both redis.Nil (no record yet) and a real error
	}
	return n >= loginMaxAttempts
}

// RecordFailure increments the failed-attempt counter for username,
// starting (or refreshing) its lockout window on the first failure.
func (l *LoginLimiter) RecordFailure(ctx context.Context, username string) {
	if l == nil || l.rdb == nil {
		return
	}
	key := loginFailKey(username)
	n, err := l.rdb.Incr(ctx, key).Result()
	if err != nil {
		return
	}
	if n == 1 {
		_ = l.rdb.Expire(ctx, key, loginLockout).Err()
	}
}

// RecordSuccess clears username's failed-attempt count — a correct password
// ends the lockout window immediately rather than making the account wait
// it out after the operator just proved they own it.
func (l *LoginLimiter) RecordSuccess(ctx context.Context, username string) {
	if l == nil || l.rdb == nil {
		return
	}
	_ = l.rdb.Del(ctx, loginFailKey(username)).Err()
}
