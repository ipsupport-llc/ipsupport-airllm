// Package unavail tracks how long to skip a (provider, upstream model) pair
// after a retryable upstream failure — independent of which alias or tier
// references it, since a target's health is a fact about the vendor/model,
// not about any one alias's fallback chain (unlike internal/breaker, which
// this does not replace — see internal/httpapi/tier_breaker.go for the
// existing per-(alias,tier) circuit breaker).
//
// There is no error-rate window, no consecutive-failure counter, and no
// half-open probe-admission race: the next real request IS the probe — if
// it also fails, it falls through to the next tier exactly like any other
// failed attempt, and the caller never knows a "probe" happened. State is
// shared via Redis so every gateway replica sees the same mark; Redis being
// unreachable fails OPEN (nothing is treated as unavailable) rather than
// blocking traffic — this is a politeness optimization, not a safety
// mechanism, so losing it under a Redis outage must never make things worse
// than before this package existed.
package unavail

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Key names one tracked target: a provider row's name and the exact
// upstream model string an alias target resolves to.
type Key struct {
	Provider      string
	UpstreamModel string
}

func (k Key) redisKey() string { return "air:unavail:" + k.Provider + ":" + k.UpstreamModel }

// record is the stored state: skip while now < UntilMS; BackoffMS is the
// duration that produced UntilMS, read back on the next failure to double
// from rather than restarting at the initial interval every time.
type record struct {
	UntilMS   int64 `json:"u"`
	BackoffMS int64 `json:"b"`
}

// recordTTL bounds how long a mark survives in Redis past being read as
// expired (`now >= UntilMS`): long enough that a slow-to-recur failure still
// doubles from its last backoff instead of restarting cold, short enough
// that a target nobody ever retries doesn't linger forever.
const recordTTL = time.Hour

// Store holds every tracked target's mark. The zero value is not ready to
// use — construct with New.
type Store struct {
	rdb   *redis.Client
	now   func() time.Time
	mu    sync.Mutex
	local map[Key]record
}

// New returns a Store backed by rdb. A nil rdb keeps all state in process
// (every replica sees only its own marks — acceptable for a single-replica
// deployment, and still strictly better than no tracking at all).
func New(rdb *redis.Client) *Store {
	return &Store{rdb: rdb, now: time.Now, local: map[Key]record{}}
}

// WithClock replaces time.Now, for tests. Returns s for chaining at
// construction.
func (s *Store) WithClock(now func() time.Time) *Store {
	s.now = now
	return s
}

// Check reports whether k is currently marked unavailable, and for how much
// longer. A Store-read failure (Redis unreachable) fails open: (0, false) —
// the caller attempts the target as if nothing were known about it.
func (s *Store) Check(ctx context.Context, k Key) (remaining time.Duration, skip bool) {
	if s == nil {
		return 0, false
	}
	r, ok := s.load(ctx, k)
	if !ok {
		return 0, false
	}
	now := s.now()
	until := time.UnixMilli(r.UntilMS)
	if now.Before(until) {
		return until.Sub(now), true
	}
	return 0, false
}

// Mark records k as unavailable and returns the duration actually used, for
// logging. retryAfter, when the upstream sent one, is honored exactly —
// that is the vendor's own statement of when it expects to be ready again,
// not a guess this gateway should second-guess with its own math. Absent
// that, the duration doubles from whatever backoff the last mark (if any,
// even an already-expired one still inside recordTTL) used, capped at max;
// with no prior mark at all, it starts at initial.
func (s *Store) Mark(ctx context.Context, k Key, retryAfter *time.Duration, initial, max time.Duration) time.Duration {
	if s == nil {
		return 0
	}
	prev, _ := s.load(ctx, k)
	var dur time.Duration
	switch {
	case retryAfter != nil:
		// The vendor's own statement of when it expects to be ready again:
		// honored exactly, never clamped by the gateway's own default max.
		dur = *retryAfter
	case prev.BackoffMS > 0:
		dur = time.Duration(prev.BackoffMS) * time.Millisecond * 2
		if max > 0 && dur > max {
			dur = max
		}
	default:
		dur = initial
	}
	if dur < 0 {
		dur = 0
	}
	now := s.now()
	s.store(ctx, k, record{UntilMS: now.Add(dur).UnixMilli(), BackoffMS: dur.Milliseconds()})
	return dur
}

// Clear removes k's mark, so the next failure starts fresh at initial
// rather than doubling from stale history.
func (s *Store) Clear(ctx context.Context, k Key) {
	if s == nil {
		return
	}
	s.mu.Lock()
	delete(s.local, k)
	s.mu.Unlock()
	if s.rdb != nil {
		_ = s.rdb.Del(ctx, k.redisKey()).Err()
	}
}

func (s *Store) load(ctx context.Context, k Key) (record, bool) {
	if s.rdb != nil {
		raw, err := s.rdb.Get(ctx, k.redisKey()).Bytes()
		switch {
		case err == nil:
			var r record
			if json.Unmarshal(raw, &r) == nil {
				return r, true
			}
			return record{}, false
		case err == redis.Nil:
			return record{}, false
		default:
			// Redis unreachable: fall through to local state rather than
			// treat this read as "no mark" — a replica that just wrote a
			// mark locally should still honor it while Redis is down.
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.local[k]
	return r, ok
}

func (s *Store) store(ctx context.Context, k Key, r record) {
	s.mu.Lock()
	s.local[k] = r
	s.mu.Unlock()
	if s.rdb == nil {
		return
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return
	}
	_ = s.rdb.Set(ctx, k.redisKey(), raw, recordTTL).Err()
}
