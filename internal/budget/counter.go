package budget

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// LedgerSum returns a user's usage recorded in the ledger in [start, end):
// tokens (prompt + completion) and cost in micro-USD.
type LedgerSum func(ctx context.Context, userID string, start, end time.Time) (tokens, costMicro int64, err error)

// Counter counts per-user usage per period in Redis and checks it against a
// user's Budgets.
//
// Each user and period has one hash, air:ub:<user>:<period-id>, holding both
// units (fields tok and cost, so they are evicted together). Settling a
// request increments the hash only if it exists. A missing hash (first use,
// or Redis lost it) is built from the ledger on the request path ("cold
// path"). A hash whose freshness marker (air:ub:<user>:fresh:<period-id>,
// FreshFor) has expired keeps being enforced while one background refresh
// re-derives it from the ledger. Building and refreshing are single-flight
// across replicas under air:ub:<user>:lock:<period-id>, and publishing
// never lowers a counter: it stores max(current, ledger sum), so increments
// that land while the ledger is read are not lost and an under-count lasts
// at most about FreshFor. An over-count (a request counted both by the
// ledger read and by its own increment) is kept until the period ends.
type Counter struct {
	rdb    *redis.Client
	loc    *time.Location
	now    func() time.Time
	ledger LedgerSum

	refreshing sync.Map       // hash key -> struct{}: background refreshes running in this process
	bg         sync.WaitGroup // background refreshes, for tests

	// afterSum, when set (tests only), runs between the ledger read and the
	// publish of a rebuild or refresh.
	afterSum func()
}

// NewCounter returns a Counter. loc nil means UTC; now nil means time.Now.
func NewCounter(rdb *redis.Client, loc *time.Location, now func() time.Time, ledger LedgerSum) *Counter {
	if loc == nil {
		loc = time.UTC
	}
	if now == nil {
		now = time.Now
	}
	return &Counter{rdb: rdb, loc: loc, now: now, ledger: ledger}
}

// Location is the zone periods are computed in.
func (c *Counter) Location() *time.Location { return c.loc }

const (
	// FreshFor is how long counters derived from the ledger are trusted
	// before a background refresh re-derives them; it bounds how long a
	// counter can stay below the ledger.
	FreshFor = 10 * time.Minute
	// counterGrace keeps counters a day past their period's end, for a
	// replica whose clock runs behind.
	counterGrace = 24 * time.Hour
	// minCounterTTL is the floor of a counter's TTL.
	minCounterTTL = time.Minute
	// retryAfter is how long a failed background refresh waits before the
	// next one, so a sick database is not asked on every request.
	retryAfter = 30 * time.Second
	// rebuildLockTTL bounds how long one replica may hold the right to
	// rebuild a user's counters for a period; ledgerTimeout stays well
	// under it.
	rebuildLockTTL = 30 * time.Second
)

// Variables so tests can shorten them.
var (
	ledgerTimeout = 15 * time.Second
	// coldWait and coldStep bound how long a cold-path check that did not
	// get the lock waits for the replica building the counters before it
	// reads the ledger itself.
	coldWait = 2 * time.Second
	coldStep = 50 * time.Millisecond
	// redisTimeout bounds the Redis calls of a background refresh and of
	// releasing a lock.
	redisTimeout = 2 * time.Second
)

// counterTTL is the TTL of counters published at now for a period ending at
// end: until a day after the period ends, never less than minCounterTTL. It
// is relative to the counter's own clock, not an absolute expiry, so a test
// clock set in the past does not create counters that vanish at once.
func counterTTL(now, end time.Time) time.Duration {
	return max(end.Sub(now)+counterGrace, minCounterTTL)
}

// The Redis keys of one user and period. They stay inside air:*, which is
// all the gateway's Redis ACL allows.
func hashKey(userID string, p Period, start time.Time) string {
	return "air:ub:" + userID + ":" + ID(p, start)
}

func lockKey(userID string, p Period, start time.Time) string {
	return "air:ub:" + userID + ":lock:" + ID(p, start)
}

func freshKey(userID string, p Period, start time.Time) string {
	return "air:ub:" + userID + ":fresh:" + ID(p, start)
}

// Denial describes the cap a request ran into.
type Denial struct {
	Period   Period
	Cost     bool  // the cost cap, else the token cap
	Limit    int64 // tokens, or micro-USD
	Used     int64
	ResetsAt time.Time
}

// Message is the client-facing text of a denial, e.g. "monthly cost budget
// $50.00 reached; resets 2026-12-01 00:00 UTC" or "daily token budget
// 30,000 reached; resets …".
func (d Denial) Message() string {
	reset := d.ResetsAt.Format("2006-01-02 15:04") + " " + d.ResetsAt.Location().String()
	if d.Cost {
		cents := (d.Limit + 5_000) / 10_000
		return fmt.Sprintf("%s cost budget $%s.%02d reached; resets %s", d.Period.adjective(), groupDigits(cents/100), cents%100, reset)
	}
	return fmt.Sprintf("%s token budget %s reached; resets %s", d.Period.adjective(), groupDigits(d.Limit), reset)
}

// groupDigits writes n with comma thousands separators: 30000 -> "30,000".
func groupDigits(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// Usage is one period's counted usage.
type Usage struct {
	Tokens    int64
	CostMicro int64
}

// errBusy marks a period another replica is building right now.
var errBusy = errors.New("budget: counters being built elsewhere")

// state is what Redis holds for one period.
type state struct {
	Usage
	have  bool // the hash exists with both fields numeric
	fresh bool // the freshness marker exists
}

// parseCounter reads one hash field; a missing or non-numeric one is not ok.
func parseCounter(v any) (int64, bool) {
	s, ok := v.(string)
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil
}

// load reads the counters and freshness markers of the periods ps at now.
// A hash with a missing or corrupt field counts as missing, so it is built
// again (and the corrupt value replaced) rather than failing every check.
func (c *Counter) load(ctx context.Context, userID string, now time.Time, ps []Period) (map[Period]state, error) {
	type cmds struct {
		vals  *redis.SliceCmd
		fresh *redis.IntCmd
	}
	pipe := c.rdb.Pipeline()
	all := make([]cmds, len(ps))
	for i, p := range ps {
		start, _ := Bounds(p, now, c.loc)
		all[i] = cmds{pipe.HMGet(ctx, hashKey(userID, p, start), "tok", "cost"), pipe.Exists(ctx, freshKey(userID, p, start))}
	}
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return nil, err
	}
	out := make(map[Period]state, len(ps))
	for i, p := range ps {
		vals := all[i].vals.Val()
		var st state
		if len(vals) == 2 {
			tok, ok1 := parseCounter(vals[0])
			cost, ok2 := parseCounter(vals[1])
			st = state{Usage: Usage{tok, cost}, have: ok1 && ok2}
		}
		st.fresh = all[i].fresh.Val() == 1
		out[p] = st
	}
	return out, nil
}

// read returns the usage of the periods ps at now.
//
// Existing counters are used as they are; stale ones (marker expired) also
// get a background refresh. Missing ones take the cold path: the replica
// that takes the lock builds them from the ledger; the others wait up to
// coldWait for its publish and, failing that, read the ledger themselves and
// use that value without writing it. A period absent from the result could
// not be read at all; the first error is returned alongside.
func (c *Counter) read(ctx context.Context, userID string, now time.Time, ps []Period) (map[Period]Usage, error) {
	states, err := c.load(ctx, userID, now, ps)
	if err != nil {
		return nil, err
	}
	out := make(map[Period]Usage, len(ps))
	var firstErr error
	keep := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	var waiting []Period
	for _, p := range ps {
		st := states[p]
		if st.have {
			out[p] = st.Usage
			if !st.fresh {
				c.refreshInBackground(userID, p, now)
			}
			continue
		}
		u, err := c.rebuild(ctx, userID, p, now)
		switch {
		case err == nil:
			out[p] = u
		case errors.Is(err, errBusy):
			waiting = append(waiting, p)
		default:
			keep(err)
		}
	}
	if len(waiting) == 0 {
		return out, firstErr
	}

	deadline := time.Now().Add(coldWait)
	for len(waiting) > 0 && time.Now().Before(deadline) && ctx.Err() == nil {
		select {
		case <-ctx.Done():
		case <-time.After(coldStep):
		}
		states, err := c.load(ctx, userID, now, waiting)
		if err != nil {
			keep(err)
			break
		}
		still := waiting[:0]
		for _, p := range waiting {
			if st := states[p]; st.have {
				out[p] = st.Usage
			} else {
				still = append(still, p)
			}
		}
		waiting = still
	}
	for _, p := range waiting {
		start, end := Bounds(p, now, c.loc)
		lctx, cancel := context.WithTimeout(ctx, ledgerTimeout)
		tok, cost, err := c.ledger(lctx, userID, start, end)
		cancel()
		if err != nil {
			keep(fmt.Errorf("budget: read ledger: %w", err))
			continue
		}
		out[p] = Usage{tok, cost}
	}
	return out, firstErr
}

// unlockScript deletes the lock only if it still holds this caller's token.
var unlockScript = redis.NewScript(`
if redis.call("get", KEYS[1]) == ARGV[1] then
	return redis.call("del", KEYS[1])
end
return 0
`)

// publishScript stores a ledger sum into a period's hash, keeping any field
// that is already higher (an increment that landed after the ledger read),
// replacing a missing or corrupt one, and sets the hash's TTL and the
// freshness marker — only while the caller still holds the lock.
// KEYS: hash, fresh marker, lock. ARGV: lock token, tok, cost, hash TTL ms,
// marker TTL ms. Returns {tok, cost} as stored, or 0 without the lock.
var publishScript = redis.NewScript(`
if redis.call("get", KEYS[3]) ~= ARGV[1] then
	return 0
end
local function pick(field, sum)
	local cur = redis.call("hget", KEYS[1], field)
	if cur and string.match(cur, "^%d+$") and tonumber(cur) >= tonumber(sum) then
		return cur
	end
	return sum
end
local tok = pick("tok", ARGV[2])
local cost = pick("cost", ARGV[3])
redis.call("hset", KEYS[1], "tok", tok, "cost", cost)
redis.call("pexpire", KEYS[1], ARGV[4])
redis.call("set", KEYS[2], "1", "PX", ARGV[5])
return {tok, cost}
`)

// rebuild derives a period's counters from the ledger and publishes them,
// holding the period's lock; without the lock it returns errBusy. If the
// counters turn out to be present and fresh once the lock is held (another
// replica just published), it returns them without reading the ledger. The
// result is what the hash holds after the publish; if the lock was lost
// meanwhile (it expired, and another replica may be publishing), nothing is
// written and the ledger sum is returned for this request to use.
func (c *Counter) rebuild(ctx context.Context, userID string, p Period, now time.Time) (Usage, error) {
	start, end := Bounds(p, now, c.loc)
	lock, token := lockKey(userID, p, start), randToken()
	ok, err := c.rdb.SetNX(ctx, lock, token, rebuildLockTTL).Result()
	if err != nil {
		return Usage{}, err
	}
	if !ok {
		return Usage{}, errBusy
	}
	defer func() {
		uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), redisTimeout)
		defer cancel()
		_ = unlockScript.Run(uctx, c.rdb, []string{lock}, token).Err()
	}()

	if st, err := c.load(ctx, userID, now, []Period{p}); err != nil {
		return Usage{}, err
	} else if st[p].have && st[p].fresh {
		return st[p].Usage, nil
	}

	lctx, cancel := context.WithTimeout(ctx, ledgerTimeout)
	tok, cost, err := c.ledger(lctx, userID, start, end)
	cancel()
	if err != nil {
		return Usage{}, fmt.Errorf("budget: rebuild from ledger: %w", err)
	}
	if c.afterSum != nil {
		c.afterSum()
	}
	res, err := publishScript.Run(ctx, c.rdb,
		[]string{hashKey(userID, p, start), freshKey(userID, p, start), lock},
		token, tok, cost, counterTTL(now, end).Milliseconds(), FreshFor.Milliseconds()).Result()
	if err != nil {
		return Usage{}, err
	}
	stored, ok := res.([]any)
	if !ok || len(stored) != 2 {
		return Usage{tok, cost}, nil
	}
	st, ok1 := parseCounter(stored[0])
	sc, ok2 := parseCounter(stored[1])
	if !ok1 || !ok2 {
		return Usage{tok, cost}, nil
	}
	return Usage{st, sc}, nil
}

// refreshInBackground re-derives a stale period's counters off the request
// path, at most once at a time per period in this process (and, through
// the lock, across replicas). A failed refresh marks the period fresh for
// retryAfter, so the next attempt waits instead of asking a sick database
// on every request.
func (c *Counter) refreshInBackground(userID string, p Period, now time.Time) {
	start, _ := Bounds(p, now, c.loc)
	key := hashKey(userID, p, start)
	if _, running := c.refreshing.LoadOrStore(key, struct{}{}); running {
		return
	}
	c.bg.Add(1)
	go func() {
		defer c.bg.Done()
		defer c.refreshing.Delete(key)
		ctx, cancel := context.WithTimeout(context.Background(), ledgerTimeout+2*redisTimeout)
		defer cancel()
		_, err := c.rebuild(ctx, userID, p, now)
		if err == nil || errors.Is(err, errBusy) {
			return
		}
		slog.Warn("budget: refreshing counters from the ledger failed; retrying later", "err", err, "user_id", userID)
		rctx, rcancel := context.WithTimeout(context.Background(), redisTimeout)
		defer rcancel()
		_ = c.rdb.Set(rctx, freshKey(userID, p, start), 1, retryAfter).Err()
	}()
}

func randToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Check reports whether userID has reached any cap in b. Longer periods are
// checked first, so a denial names the cap that keeps the user blocked the
// longest. A period whose usage could not be read at all is not enforced
// for this request; the error is returned for the caller to log.
func (c *Counter) Check(ctx context.Context, userID string, b Budgets) (Denial, bool, error) {
	if userID == "" || !b.Any() {
		return Denial{}, false, nil
	}
	now := c.now()
	var ps []Period
	for i := len(Periods) - 1; i >= 0; i-- {
		if p := Periods[i]; b.TokenCap(p) > 0 || b.CostCapMicro(p) > 0 {
			ps = append(ps, p)
		}
	}
	used, err := c.read(ctx, userID, now, ps)
	for _, p := range ps {
		u, ok := used[p]
		if !ok {
			continue
		}
		_, end := Bounds(p, now, c.loc)
		if lim := b.TokenCap(p); lim > 0 && u.Tokens >= lim {
			return Denial{Period: p, Limit: lim, Used: u.Tokens, ResetsAt: end}, true, err
		}
		if lim := b.CostCapMicro(p); lim > 0 && u.CostMicro >= lim {
			return Denial{Period: p, Cost: true, Limit: lim, Used: u.CostMicro, ResetsAt: end}, true, err
		}
	}
	return Denial{}, false, err
}

// Usage returns the user's usage in the day, week and month containing at,
// read the same way Check reads it.
func (c *Counter) Usage(ctx context.Context, userID string, at time.Time) (map[Period]Usage, error) {
	used, err := c.read(ctx, userID, at, Periods)
	if err != nil {
		return nil, err
	}
	if len(used) < len(Periods) {
		return nil, errBusy
	}
	return used, nil
}

// addScript increments each period's hash that already exists and leaves
// missing ones alone: a missing hash is built from the ledger, in full, by
// the next read, and creating it here would make it hold only this request.
// HINCRBY keeps the hash's TTL. A field that cannot be incremented (corrupt)
// is skipped; the read treats that hash as missing and rebuilds it.
// ARGV[1] is the token delta, ARGV[2] the cost delta.
var addScript = redis.NewScript(`
for _, k in ipairs(KEYS) do
	if redis.call("exists", k) == 1 then
		if ARGV[1] ~= "0" then redis.pcall("hincrby", k, "tok", ARGV[1]) end
		if ARGV[2] ~= "0" then redis.pcall("hincrby", k, "cost", ARGV[2]) end
	end
end
return 0
`)

// Add records a settled request's usage against the counters of the periods
// containing at — the same instant its ledger row is stamped with, so the
// counter and the ledger agree on which day, week and month it belongs to.
func (c *Counter) Add(ctx context.Context, userID string, at time.Time, tokens, costMicro int64) error {
	if userID == "" || (tokens == 0 && costMicro == 0) {
		return nil
	}
	keys := make([]string, 0, len(Periods))
	for _, p := range Periods {
		start, _ := Bounds(p, at, c.loc)
		keys = append(keys, hashKey(userID, p, start))
	}
	return addScript.Run(ctx, c.rdb, keys, strconv.FormatInt(tokens, 10), strconv.FormatInt(costMicro, 10)).Err()
}
