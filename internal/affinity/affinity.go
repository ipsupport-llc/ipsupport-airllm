// Package affinity remembers which tier of an alias each client session has
// been moved to, so that a session served by a backup tier starts there on
// its next requests instead of going back to the primary — a phone call that
// switched voice keeps the new voice until it ends.
//
// A pin only ever moves forward: pinning a session to an earlier tier than
// the one it holds keeps the later tier and only refreshes the expiry.
//
// Pins live in Redis, so every gateway replica sees them. Each replica also
// keeps the pins it wrote itself, which carries its own sessions through a
// Redis outage; reads take the later of the two tiers.
package affinity

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Store holds the session pins. The zero of *Store (nil) remembers nothing.
type Store struct {
	now       func() time.Time
	rdb       *redis.Client // nil keeps pins in this process only
	opTimeout time.Duration

	mu    sync.Mutex
	local map[key]pin
	// nextSweep is when a write may next drop the expired local pins.
	nextSweep time.Time
	// remoteDownTill is when to try Redis again after it failed. It runs on
	// the wall clock, not now: it is about the network, not pin expiry.
	remoteDownTill time.Time
}

type key struct{ alias, session string }

type pin struct {
	tier  int
	until time.Time
}

// Option configures a Store.
type Option func(*Store)

// WithClock replaces time.Now for the local pins' expiry, for tests.
func WithClock(now func() time.Time) Option { return func(s *Store) { s.now = now } }

// remoteRetryAfter is how long a replica stays on its own pins after a Redis
// error before trying Redis again, so an outage costs one timeout per
// interval rather than one per request.
const remoteRetryAfter = 5 * time.Second

// sweepAt is the local pin count past which a write drops the expired ones,
// at most once per sweepEvery, so a long-running replica does not keep every
// call it ever served nor walk them all on every write.
const (
	sweepAt    = 1024
	sweepEvery = time.Minute
)

// New returns a store whose pins are shared through rdb; nil rdb keeps them
// in this process.
func New(rdb *redis.Client, opts ...Option) *Store {
	s := &Store{now: time.Now, rdb: rdb, opTimeout: 250 * time.Millisecond, local: map[key]pin{}}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Pinned returns the tier session is pinned to on alias, if any.
func (s *Store) Pinned(ctx context.Context, alias, session string) (int, bool) {
	if s == nil {
		return 0, false
	}
	k := key{alias, session}
	tier, ok := s.localPinned(k)
	if s.useRemote() {
		cctx, cancel := context.WithTimeout(ctx, s.opTimeout)
		defer cancel()
		v, err := s.rdb.Get(cctx, redisKey(k)).Result()
		switch {
		case err == nil:
			s.remoteOK()
			if rt, perr := strconv.Atoi(v); perr == nil && (!ok || rt > tier) {
				tier, ok = rt, true
			}
		case errors.Is(err, redis.Nil):
			s.remoteOK()
		case ctx.Err() == nil:
			s.remoteFailed(err)
		}
	}
	return tier, ok
}

// Pin records that session was served by tier on alias, for ttl from now. A
// session already pinned to a later tier keeps it; either way the expiry is
// renewed. moved reports that the session's tier changed.
func (s *Store) Pin(ctx context.Context, alias, session string, tier int, ttl time.Duration) (moved bool) {
	if s == nil {
		return false
	}
	k := key{alias, session}
	moved = s.localPin(k, tier, ttl)
	if s.useRemote() {
		cctx, cancel := context.WithTimeout(ctx, s.opTimeout)
		defer cancel()
		prev, err := pinScript.Run(cctx, s.rdb, []string{redisKey(k)}, tier, ttl.Milliseconds()).Int()
		switch {
		case err == nil:
			s.remoteOK()
			moved = prev < tier
		case ctx.Err() == nil:
			s.remoteFailed(err)
		}
	}
	return moved
}

// Forget drops session's pin on alias, so its next pin starts afresh rather
// than being held back by a later tier.
func (s *Store) Forget(ctx context.Context, alias, session string) {
	if s == nil {
		return
	}
	k := key{alias, session}
	s.mu.Lock()
	delete(s.local, k)
	s.mu.Unlock()
	if s.useRemote() {
		cctx, cancel := context.WithTimeout(ctx, s.opTimeout)
		defer cancel()
		switch err := s.rdb.Del(cctx, redisKey(k)).Err(); {
		case err == nil:
			s.remoteOK()
		case ctx.Err() == nil:
			s.remoteFailed(err)
		}
	}
}

func (s *Store) localPinned(k key) (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.local[k]
	if !ok || !s.now().Before(p.until) {
		return 0, false
	}
	return p.tier, true
}

func (s *Store) localPin(k key, tier int, ttl time.Duration) (moved bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if len(s.local) >= sweepAt && !now.Before(s.nextSweep) {
		s.nextSweep = now.Add(sweepEvery)
		for lk, p := range s.local {
			if !now.Before(p.until) {
				delete(s.local, lk)
			}
		}
	}
	p, ok := s.local[k]
	if ok && now.Before(p.until) && p.tier >= tier {
		tier = p.tier
	} else {
		moved = true
	}
	s.local[k] = pin{tier: tier, until: now.Add(ttl)}
	return moved
}

func (s *Store) useRemote() bool {
	if s.rdb == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Now().After(s.remoteDownTill)
}

func (s *Store) remoteFailed(err error) {
	s.mu.Lock()
	first := s.remoteDownTill.IsZero()
	s.remoteDownTill = time.Now().Add(remoteRetryAfter)
	s.mu.Unlock()
	if first {
		slog.Warn("affinity: shared session pins unavailable, using local pins", "err", err)
	}
}

func (s *Store) remoteOK() {
	s.mu.Lock()
	recovered := !s.remoteDownTill.IsZero()
	s.remoteDownTill = time.Time{}
	s.mu.Unlock()
	if recovered {
		slog.Info("affinity: shared session pins reachable again")
	}
}

// redisKey names one session's pin. The session is client-chosen, so it is
// hashed: the key has a fixed shape whatever the client sent.
func redisKey(k key) string {
	sum := sha256.Sum256([]byte(k.session))
	return "airllm:affinity:" + k.alias + ":" + hex.EncodeToString(sum[:])
}

// pinScript raises a pin to ARGV[1] unless it already holds a later tier,
// and renews its expiry to ARGV[2] ms either way. It returns the tier held
// before, or -1 for none, so the caller can tell whether the pin moved.
var pinScript = redis.NewScript(`
local cur = tonumber(redis.call('GET', KEYS[1]))
local tier = tonumber(ARGV[1])
if cur and cur >= tier then
  redis.call('PEXPIRE', KEYS[1], ARGV[2])
  return cur
end
redis.call('SET', KEYS[1], ARGV[1], 'PX', ARGV[2])
return cur or -1
`)
