// Package lcr tracks a lightweight, per-(provider, upstream_model)
// exponential moving average of request success, for the LCR-style
// `availability`/`both` within-tier ordering strategy (see
// docs/superpowers/specs/2026-10-10-lcr-routing-design.md). Deliberately
// much simpler than internal/breaker: no windows, no counters, no admission
// state — just a single float per target, read in a batch before ordering
// and written fire-and-forget after each attempt's outcome is known.
//
// A target with no recorded history is treated as fully available (1.0):
// optimistic cold start, matching internal/unavail's existing "never
// penalized until it actually fails" philosophy. The stored record carries
// a TTL shorter than internal/unavail's hour-long one, specifically so a
// target that stops receiving attempts (because it keeps losing the
// ordering to healthier peers) periodically resets to cold-start and
// becomes competitive again — without this, a target demoted once could
// never earn a new observation to recover from, a starvation bug both
// design reviewers (codex and muse) independently flagged.
package lcr

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Key identifies the target an availability figure belongs to.
type Key struct {
	Provider      string
	UpstreamModel string
}

func (k Key) redisKey() string { return "air:lcr:avail:" + k.Provider + ":" + k.UpstreamModel }

// alpha is the EWMA smoothing factor: avail = alpha*outcome + (1-alpha)*prev.
// Fixed, not configurable — see the spec's Global Constraints.
const alpha = 0.1

// recordTTL is how long a mark survives without a new observation before the
// next read sees "no history" and treats the target as cold-started again.
const recordTTL = 15 * time.Minute

// opTimeout bounds every individual Redis call, matching internal/breaker's
// discipline (internal/unavail predates this convention and lacks it).
const opTimeout = 250 * time.Millisecond

// remoteRetryAfter is how long Snapshot/Record skip Redis entirely after a
// failure, so a sustained outage costs one timeout per interval instead of
// one per call — mirrors internal/breaker's remoteDownTill/remoteFailed/
// remoteOK pattern (see breaker.go).
const remoteRetryAfter = 5 * time.Second

type record struct {
	Avail float64 `json:"a"`
}

// Store tracks EWMA availability in Redis, shared across replicas, failing
// open to optimistic (1.0) availability when Redis is unreachable.
type Store struct {
	rdb *redis.Client
	now func() time.Time

	mu sync.Mutex
	// remoteDownTill is when to try Redis again after it failed. It runs on
	// the wall clock, not now: it is about the network, not EWMA time.
	remoteDownTill time.Time
}

// New returns a Store. A nil rdb is valid: every Snapshot then returns 1.0
// for every key and Record is a no-op, exactly like an unreachable Redis.
func New(rdb *redis.Client) *Store { return &Store{rdb: rdb, now: time.Now} }

// WithClock overrides the clock for deterministic tests. Returns the
// receiver for chaining.
func (s *Store) WithClock(now func() time.Time) *Store { s.now = now; return s }

// Snapshot reads the current availability of every key in one pipelined
// round trip. A key with no stored record, an unparsable one, or any Redis
// error is reported as 1.0 (fail open / cold start) — Snapshot itself never
// returns an error.
func (s *Store) Snapshot(ctx context.Context, keys []Key) map[Key]float64 {
	out := make(map[Key]float64, len(keys))
	for _, k := range keys {
		out[k] = 1.0
	}
	if s == nil || s.rdb == nil || len(keys) == 0 {
		return out
	}
	if !s.useRemote() {
		// A prior failure's backoff window is still running: skip Redis
		// entirely rather than pay opTimeout again for a call almost
		// certain to fail the same way.
		return out
	}
	cctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	pipe := s.rdb.Pipeline()
	cmds := make(map[Key]*redis.StringCmd, len(keys))
	for _, k := range keys {
		cmds[k] = pipe.Get(cctx, k.redisKey())
	}
	if _, err := pipe.Exec(cctx); err != nil && err != redis.Nil {
		s.remoteFailed(err)
		return out
	}
	s.remoteOK()
	for k, cmd := range cmds {
		raw, err := cmd.Bytes()
		if err != nil {
			continue // redis.Nil (no record) or a transport error: stays 1.0
		}
		var rec record
		if err := json.Unmarshal(raw, &rec); err != nil {
			continue
		}
		out[k] = rec.Avail
	}
	return out
}

// Record updates k's EWMA with one outcome and refreshes its TTL. The write
// happens in a detached goroutine with its own bounded context: Record
// itself returns immediately, so calling it from a streaming response's
// first-chunk commit callback never delays that chunk's delivery. A race
// between two concurrent Records on the same key can lose one update — an
// explicitly accepted tradeoff for a statistical smoothing signal, unlike
// internal/breaker's correctness-critical counters.
func (s *Store) Record(ctx context.Context, k Key, success bool) {
	if s == nil || s.rdb == nil {
		return
	}
	go s.record(context.WithoutCancel(ctx), k, success)
}

func (s *Store) record(ctx context.Context, k Key, success bool) {
	if !s.useRemote() {
		// Same backoff as Snapshot: skip Redis entirely during an outage
		// rather than pay opTimeout on a background goroutine nobody waits
		// on anyway.
		return
	}
	cctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	prev := 1.0
	raw, err := s.rdb.Get(cctx, k.redisKey()).Bytes()
	if err != nil && !errors.Is(err, redis.Nil) {
		s.remoteFailed(err)
		return
	}
	if err == nil {
		var rec record
		if json.Unmarshal(raw, &rec) == nil {
			prev = rec.Avail
		}
	}
	outcome := 0.0
	if success {
		outcome = 1.0
	}
	next := alpha*outcome + (1-alpha)*prev
	out, err := json.Marshal(record{Avail: next})
	if err != nil {
		return
	}
	if err := s.rdb.Set(cctx, k.redisKey(), out, recordTTL).Err(); err != nil {
		s.remoteFailed(err)
		return
	}
	s.remoteOK()
}

// useRemote reports whether Redis should be tried right now: false while a
// previous failure's backoff window is still running.
func (s *Store) useRemote() bool {
	if s.rdb == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Now().After(s.remoteDownTill)
}

// remoteFailed starts (or extends) the backoff window after a Redis error,
// logging only on the transition into the down state — not on every
// subsequent call skipped because of it.
func (s *Store) remoteFailed(err error) {
	s.mu.Lock()
	first := s.remoteDownTill.IsZero()
	s.remoteDownTill = time.Now().Add(remoteRetryAfter)
	s.mu.Unlock()
	if first {
		slog.Warn("lcr: Redis unavailable, failing open to optimistic availability", "err", err)
	}
}

// remoteOK clears the backoff window after a successful Redis call, logging
// only when it actually recovered from a down state.
func (s *Store) remoteOK() {
	s.mu.Lock()
	recovered := !s.remoteDownTill.IsZero()
	s.remoteDownTill = time.Time{}
	s.mu.Unlock()
	if recovered {
		slog.Info("lcr: Redis reachable again")
	}
}
