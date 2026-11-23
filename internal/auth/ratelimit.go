package auth

import (
	"context"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// loginMaxAttempts is how many consecutive failed logins a username may have
// from one client IP before LoginLimiter locks that pair out; loginLockout is
// how long the lockout lasts once triggered.
const (
	loginMaxAttempts = 10
	loginLockout     = 15 * time.Minute
)

// LoginLimiter throttles repeated failed login attempts per username and
// client IP, so a brute-force guesser can't try passwords without bound. The
// IP is part of the key so that someone who merely knows a username (say
// "admin") can't lock its owner out by failing logins from elsewhere. Backed by Redis
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

// loginFailKey keys the counter by client IP and username. The IP goes
// first: it never contains "|", so the key is unambiguous for any username.
func loginFailKey(username, clientIP string) string {
	return "air:loginfail:" + clientIP + "|" + strings.ToLower(username)
}

// Locked reports whether username is currently locked out for clientIP from
// too many recent failed attempts. Fails open (not locked) on a Redis error —
// a Redis outage must not be able to lock every operator out of the console.
func (l *LoginLimiter) Locked(ctx context.Context, username, clientIP string) bool {
	if l == nil || l.rdb == nil {
		return false
	}
	n, err := l.rdb.Get(ctx, loginFailKey(username, clientIP)).Int()
	if err != nil {
		return false // covers both redis.Nil (no record yet) and a real error
	}
	return n >= loginMaxAttempts
}

// RecordFailure increments the failed-attempt counter for username from
// clientIP, starting (or refreshing) its lockout window on the first failure.
func (l *LoginLimiter) RecordFailure(ctx context.Context, username, clientIP string) {
	if l == nil || l.rdb == nil {
		return
	}
	key := loginFailKey(username, clientIP)
	n, err := l.rdb.Incr(ctx, key).Result()
	if err != nil {
		return
	}
	if n == 1 {
		_ = l.rdb.Expire(ctx, key, loginLockout).Err()
	}
}

// RecordSuccess clears username's failed-attempt count for clientIP — a
// correct password ends the lockout window immediately rather than making
// the account wait it out after the operator just proved they own it.
func (l *LoginLimiter) RecordSuccess(ctx context.Context, username, clientIP string) {
	if l == nil || l.rdb == nil {
		return
	}
	_ = l.rdb.Del(ctx, loginFailKey(username, clientIP)).Err()
}
