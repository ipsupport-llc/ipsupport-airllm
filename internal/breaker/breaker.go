// Package breaker is the circuit breaker the executor puts in front of each
// tier of an alias. A tier that keeps failing is opened (quarantined) for a
// cooldown, so requests skip it instead of paying its timeout again; after the
// cooldown one probe request is let through, and its outcome closes the tier
// or re-opens it with a longer cooldown.
//
// State lives in Redis, so every gateway replica sees the same breaker. When
// Redis cannot be reached each replica falls back to its own in-memory state
// and keeps serving; it goes back to Redis once Redis answers again.
package breaker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Settings are the thresholds one tier's breaker runs under.
type Settings struct {
	// Enabled switches the breaker on. A disabled tier is never skipped and
	// its outcomes are not recorded.
	Enabled bool
	// Failures is the run of consecutive failures that opens the tier.
	Failures int
	// ErrorRate opens the tier when more than this fraction of the requests
	// in the current Window failed, once MinRequests were seen in it.
	ErrorRate   float64
	Window      time.Duration
	MinRequests int
	// Cooldown is how long the first trip keeps the tier open. Each re-trip
	// doubles it, up to MaxCooldown; it falls back to Cooldown once the tier
	// has stayed closed for Stable.
	Cooldown    time.Duration
	MaxCooldown time.Duration
	Stable      time.Duration
	// ProbeLease is how long an admitted probe holds the tier before another
	// one may go: long enough for the attempt to finish. Below the 30s
	// floor, the floor applies.
	ProbeLease time.Duration
}

// Defaults are the starting thresholds: three consecutive failures, or more
// than half failing over thirty seconds with at least five requests; a
// sixty-second cooldown doubling to thirty minutes; reset after ten stable
// minutes. Enabled is left off.
func Defaults() Settings {
	return Settings{
		Failures:    3,
		ErrorRate:   0.5,
		Window:      30 * time.Second,
		MinRequests: 5,
		Cooldown:    time.Minute,
		MaxCooldown: 30 * time.Minute,
		Stable:      10 * time.Minute,
	}
}

// minProbeLease is the shortest an admitted probe holds the tier: if its
// outcome is never recorded (the replica running it died), another probe is
// admitted after the lease.
const minProbeLease = 30 * time.Second

func (set Settings) probeLease() time.Duration {
	return max(set.ProbeLease, minProbeLease)
}

// coolingDown reports whether the tier must still be skipped at now: open
// with its cooldown running, or probing with the probe's lease running.
func (r *record) coolingDown(now time.Time) bool {
	switch r.state() {
	case Open:
		return now.UnixMilli() < r.OpenUntil
	case HalfOpen:
		return now.UnixMilli() < r.ProbeUntil
	}
	return false
}

// Key names one breaker: a tier of an alias. Tier is the configured tier
// priority, which stays stable when other tiers are added or disabled.
type Key struct {
	Alias string
	Tier  int
}

func (k Key) String() string { return k.Alias + "#" + strconv.Itoa(k.Tier) }

// State is where a breaker is in its cycle.
type State string

const (
	Closed   State = "closed"
	Open     State = "open"
	HalfOpen State = "half_open"
)

// Trip reasons, as reported on Status and transitions.
const (
	ReasonConsecutive = "consecutive_failures"
	ReasonErrorRate   = "error_rate"
	ReasonProbeFailed = "probe_failed"
)

// Admission is the breaker's answer for one attempt at a tier.
type Admission struct {
	// Skip means the tier is open: do not call it.
	Skip bool
	// Probe means this attempt is the single probe of a tier whose cooldown
	// ran out. Its outcome must be recorded with Record(probe=true), or
	// handed back with AbandonProbe if no call was made.
	Probe bool
}

// Status is a tier's breaker as an operator sees it.
type Status struct {
	State               State     `json:"state"`
	Reason              string    `json:"reason,omitempty"`
	OpenUntil           time.Time `json:"open_until,omitzero"`
	CooldownMS          int64     `json:"cooldown_ms"`
	Trips               int       `json:"trips"`
	ConsecutiveFailures int       `json:"consecutive_failures"`
	WindowRequests      int       `json:"window_requests"`
	WindowFailures      int       `json:"window_failures"`
}

// Transition is one change of a breaker's state, reported to the observer by
// the replica that made it — so each transition is reported once.
type Transition struct {
	Key  Key
	From State
	To   State
	// Cause is the trip reason for an opening, "probe" or "manual" for a
	// closing, and empty for a probe admission.
	Cause    string
	Cooldown time.Duration
	// OpenUntil is set for an opening.
	OpenUntil time.Time
}

// record is the stored state of one breaker. Times are Unix milliseconds.
type record struct {
	State       State  `json:"s"`
	Reason      string `json:"r,omitempty"`
	Consecutive int    `json:"c,omitempty"`
	WindowStart int64  `json:"ws,omitempty"`
	WindowReqs  int    `json:"wr,omitempty"`
	WindowFails int    `json:"wf,omitempty"`
	Trips       int    `json:"t,omitempty"`
	CooldownMS  int64  `json:"cd,omitempty"`
	OpenUntil   int64  `json:"ou,omitempty"`
	ProbeUntil  int64  `json:"pu,omitempty"`
	ClosedAt    int64  `json:"ca,omitempty"`
}

func (r *record) state() State {
	if r.State == "" {
		return Closed
	}
	return r.State
}

// Breaker holds the breakers of every alias tier. The zero of *Breaker (nil)
// admits everything and records nothing.
type Breaker struct {
	now       func() time.Time
	observe   func(Transition)
	local     *memStore
	remote    *redisStore // nil without Redis
	opTimeout time.Duration

	// seen is every key this replica admitted with the breaker on, with the
	// settings it last ran under: what the state metric reports on.
	seen sync.Map // Key -> seenKey

	mu sync.Mutex
	// remoteDownTill is when to try Redis again after it failed. It runs on
	// the wall clock, not now: it is about the network, not breaker time.
	remoteDownTill time.Time
}

// Option configures a Breaker.
type Option func(*Breaker)

// WithClock replaces time.Now, for tests.
func WithClock(now func() time.Time) Option { return func(b *Breaker) { b.now = now } }

// WithObserver receives every transition this replica makes.
func WithObserver(fn func(Transition)) Option { return func(b *Breaker) { b.observe = fn } }

// remoteRetryAfter is how long a replica stays on its local state after a
// Redis error before trying Redis again, so an outage costs one timeout per
// interval rather than one per request.
const remoteRetryAfter = 5 * time.Second

// New returns a breaker whose state is shared through rdb; nil rdb keeps all
// state in this process.
func New(rdb *redis.Client, opts ...Option) *Breaker {
	b := &Breaker{now: time.Now, local: newMemStore(), opTimeout: 250 * time.Millisecond}
	if rdb != nil {
		b.remote = &redisStore{rdb: rdb}
	}
	for _, o := range opts {
		o(b)
	}
	return b
}

// Admit decides whether one attempt may call the tier.
func (b *Breaker) Admit(ctx context.Context, k Key, set Settings) Admission {
	if b == nil || !set.Enabled {
		return Admission{}
	}
	now := b.now()
	b.seen.Store(k, seenKey{set: set, at: now})
	cur, err := b.load(ctx, k)
	if err != nil {
		return Admission{}
	}
	if cur.state() == Closed {
		return Admission{}
	}
	if cur.coolingDown(now) {
		return Admission{Skip: true}
	}
	// The cooldown (or a lost probe's lease) has run out: race the other
	// requests for the single probe.
	var adm Admission
	var tr *Transition
	err = b.update(ctx, k, set, func(r *record) bool {
		adm, tr = Admission{}, nil
		if r.state() == Closed {
			return false
		}
		if r.coolingDown(now) {
			adm.Skip = true
			return false
		}
		tr = &Transition{Key: k, From: r.state(), To: HalfOpen}
		r.State = HalfOpen
		r.ProbeUntil = now.Add(set.probeLease()).UnixMilli()
		adm.Probe = true
		return true
	})
	if errors.Is(err, errContended) {
		// Other requests kept winning the race for this record: one of
		// them holds the probe.
		return Admission{Skip: true}
	}
	if err != nil {
		return Admission{}
	}
	if tr != nil {
		b.emit(*tr)
	}
	return adm
}

// Record reports the outcome of an attempt Admit let through. failed is
// whether it failed in a way that counts against the tier's health; probe is
// the Admission's Probe. An attempt that says nothing about the tier (it
// failed because of the request itself) is not recorded at all — a probe
// like that is handed back with AbandonProbe.
func (b *Breaker) Record(ctx context.Context, k Key, set Settings, failed, probe bool) {
	if b == nil || !set.Enabled {
		return
	}
	now := b.now()
	var tr *Transition
	_ = b.update(ctx, k, set, func(r *record) bool {
		tr = nil
		switch r.state() {
		case HalfOpen:
			if !probe {
				return false
			}
			if failed {
				tr = trip(r, k, set, now, ReasonProbeFailed)
			} else {
				tr = &Transition{Key: k, From: HalfOpen, To: Closed, Cause: "probe"}
				*r = record{State: Closed, Trips: r.Trips, CooldownMS: r.CooldownMS, ClosedAt: now.UnixMilli()}
			}
			return true
		case Open:
			// An attempt that was already in flight when the tier opened.
			return false
		}
		if r.WindowStart == 0 || now.Sub(time.UnixMilli(r.WindowStart)) >= set.Window {
			r.WindowStart, r.WindowReqs, r.WindowFails = now.UnixMilli(), 0, 0
		}
		r.WindowReqs++
		if !failed {
			r.Consecutive = 0
			return true
		}
		r.Consecutive++
		r.WindowFails++
		switch {
		case set.Failures > 0 && r.Consecutive >= set.Failures:
			tr = trip(r, k, set, now, ReasonConsecutive)
		case set.MinRequests > 0 && r.WindowReqs >= set.MinRequests && float64(r.WindowFails) > set.ErrorRate*float64(r.WindowReqs):
			tr = trip(r, k, set, now, ReasonErrorRate)
		}
		return true
	})
	if tr != nil {
		b.emit(*tr)
	}
}

// AbandonProbe hands back a probe that made no call (the tier was busy, or
// the client went away first), so the next request can probe at once.
func (b *Breaker) AbandonProbe(ctx context.Context, k Key, set Settings) {
	if b == nil || !set.Enabled {
		return
	}
	_ = b.update(ctx, k, set, func(r *record) bool {
		if r.state() != HalfOpen {
			return false
		}
		r.ProbeUntil = 0
		return true
	})
}

// Status reports a tier's breaker.
func (b *Breaker) Status(ctx context.Context, k Key, set Settings) (Status, error) {
	if b == nil {
		return Status{State: Closed}, nil
	}
	r, err := b.load(ctx, k)
	if err != nil {
		return Status{}, err
	}
	st := Status{
		State: r.state(), Reason: r.Reason, Trips: r.Trips, CooldownMS: r.CooldownMS,
		ConsecutiveFailures: r.Consecutive, WindowRequests: r.WindowReqs, WindowFailures: r.WindowFails,
	}
	switch st.State {
	case Closed:
		st.Reason = ""
		if r.ClosedAt > 0 && b.now().Sub(time.UnixMilli(r.ClosedAt)) >= set.Stable {
			st.Trips, st.CooldownMS = 0, 0
		}
	case Open:
		st.OpenUntil = time.UnixMilli(r.OpenUntil).UTC()
	}
	return st, nil
}

// seenKey is when a key was last admitted, and under which settings.
type seenKey struct {
	set Settings
	at  time.Time
}

// Seen calls fn for every tier this replica has admitted requests to with
// the breaker on, with the settings it last ran under. A tier not admitted
// for as long as its state could outlive (an alias or tier since deleted,
// say) is forgotten.
func (b *Breaker) Seen(fn func(Key, Settings)) {
	if b == nil {
		return
	}
	now := b.now()
	b.seen.Range(func(k, v any) bool {
		sk := v.(seenKey)
		if now.Sub(sk.at) > ttl(sk.set) {
			b.seen.Delete(k)
			return true
		}
		fn(k.(Key), sk.set)
		return true
	})
}

// Release closes a tier by hand and forgets its history, so its next trip
// starts at the initial cooldown.
func (b *Breaker) Release(ctx context.Context, k Key) error {
	if b == nil {
		return nil
	}
	r, err := b.load(ctx, k)
	if err != nil {
		return err
	}
	b.local.del(k)
	// Always reach for the shared state, even while this replica is backing
	// off from Redis: releasing only the local copy would leave every other
	// replica skipping the tier.
	if b.remote != nil {
		cctx, cancel := context.WithTimeout(ctx, b.opTimeout)
		defer cancel()
		if err := b.remote.del(cctx, k); err != nil {
			b.remoteFailed(err)
			return err
		}
	}
	if r.state() != Closed {
		b.emit(Transition{Key: k, From: r.state(), To: Closed, Cause: "manual"})
	}
	return nil
}

// trip opens the tier with the next cooldown and returns the transition.
func trip(r *record, k Key, set Settings, now time.Time, reason string) *Transition {
	from := r.state()
	trips := r.Trips
	// A tier that stayed closed for the stable period starts over.
	if from == Closed && r.ClosedAt > 0 && now.Sub(time.UnixMilli(r.ClosedAt)) >= set.Stable {
		trips = 0
	}
	trips++
	cooldown := set.Cooldown
	for i := 1; i < trips && cooldown < set.MaxCooldown; i++ {
		cooldown *= 2
	}
	if set.MaxCooldown > 0 && cooldown > set.MaxCooldown {
		cooldown = set.MaxCooldown
	}
	until := now.Add(cooldown)
	*r = record{State: Open, Reason: reason, Trips: trips, CooldownMS: cooldown.Milliseconds(), OpenUntil: until.UnixMilli(), ClosedAt: r.ClosedAt}
	return &Transition{Key: k, From: from, To: Open, Cause: reason, Cooldown: cooldown, OpenUntil: until}
}

func (b *Breaker) emit(t Transition) {
	if b.observe != nil {
		b.observe(t)
	}
}

// ttl bounds how long a breaker's state outlives its last change: long enough
// for the longest cooldown, a probe and the stable period to play out.
func ttl(set Settings) time.Duration {
	return set.MaxCooldown + set.Stable + set.Window + set.probeLease() + time.Minute
}

func (b *Breaker) useRemote() bool {
	if b.remote == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return time.Now().After(b.remoteDownTill)
}

func (b *Breaker) remoteFailed(err error) {
	b.mu.Lock()
	first := b.remoteDownTill.IsZero()
	b.remoteDownTill = time.Now().Add(remoteRetryAfter)
	b.mu.Unlock()
	if first {
		slog.Warn("breaker: shared state unavailable, using local state", "err", err)
	}
}

func (b *Breaker) remoteOK() {
	b.mu.Lock()
	recovered := !b.remoteDownTill.IsZero()
	b.remoteDownTill = time.Time{}
	b.mu.Unlock()
	if recovered {
		// What this replica decided alone is stale now that the shared
		// state is back; a later outage must not resurrect it.
		b.local.reset()
		slog.Info("breaker: shared state reachable again")
	}
}

func (b *Breaker) load(ctx context.Context, k Key) (record, error) {
	if b.useRemote() {
		cctx, cancel := context.WithTimeout(ctx, b.opTimeout)
		defer cancel()
		r, err := b.remote.load(cctx, k)
		if err == nil {
			b.remoteOK()
			return r, nil
		}
		if ctx.Err() != nil {
			return record{}, ctx.Err()
		}
		b.remoteFailed(err)
	}
	return b.local.load(k), nil
}

func (b *Breaker) update(ctx context.Context, k Key, set Settings, fn func(*record) bool) error {
	if b.useRemote() {
		cctx, cancel := context.WithTimeout(ctx, b.opTimeout)
		defer cancel()
		err := b.remote.update(cctx, k, ttl(set), fn)
		if err == nil {
			b.remoteOK()
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, errContended) {
			return err
		}
		b.remoteFailed(err)
	}
	b.local.update(k, fn)
	return nil
}

// memStore is the in-process state: the only state without Redis, and the
// fallback while Redis is unreachable.
type memStore struct {
	mu   sync.Mutex
	recs map[Key]record
}

func newMemStore() *memStore { return &memStore{recs: map[Key]record{}} }

func (m *memStore) load(k Key) record {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.recs[k]
}

func (m *memStore) update(k Key, fn func(*record) bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.recs[k]
	if fn(&r) {
		m.recs[k] = r
	}
}

func (m *memStore) reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recs = map[Key]record{}
}

func (m *memStore) del(k Key) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.recs, k)
}

// redisStore keeps each breaker as one JSON value, changed with optimistic
// transactions so concurrent replicas never lose each other's updates.
type redisStore struct {
	rdb *redis.Client
}

// errContended is a transaction that lost its race too many times in a row.
// Redis itself is fine, so it does not send the replica to local state.
var errContended = errors.New("breaker: state update contended")

const updateAttempts = 5

func redisKey(k Key) string {
	return fmt.Sprintf("air:breaker:%s:%d", k.Alias, k.Tier)
}

func (s *redisStore) load(ctx context.Context, k Key) (record, error) {
	raw, err := s.rdb.Get(ctx, redisKey(k)).Bytes()
	if errors.Is(err, redis.Nil) {
		return record{}, nil
	}
	if err != nil {
		return record{}, err
	}
	var r record
	if err := json.Unmarshal(raw, &r); err != nil {
		return record{}, nil
	}
	return r, nil
}

func (s *redisStore) update(ctx context.Context, k Key, ttl time.Duration, fn func(*record) bool) error {
	key := redisKey(k)
	for i := 0; i < updateAttempts; i++ {
		err := s.rdb.Watch(ctx, func(tx *redis.Tx) error {
			var r record
			raw, err := tx.Get(ctx, key).Bytes()
			switch {
			case errors.Is(err, redis.Nil):
			case err != nil:
				return err
			default:
				_ = json.Unmarshal(raw, &r)
			}
			if !fn(&r) {
				return nil
			}
			out, _ := json.Marshal(r)
			_, err = tx.TxPipelined(ctx, func(p redis.Pipeliner) error {
				p.Set(ctx, key, out, ttl)
				return nil
			})
			return err
		}, key)
		if !errors.Is(err, redis.TxFailedErr) {
			return err
		}
	}
	return errContended
}

func (s *redisStore) del(ctx context.Context, k Key) error {
	return s.rdb.Del(ctx, redisKey(k)).Err()
}
