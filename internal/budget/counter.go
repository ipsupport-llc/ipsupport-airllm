package budget

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// LedgerSum returns a user's usage recorded in the ledger in [start, end):
// tokens (prompt + completion) and cost in micro-USD.
type LedgerSum func(ctx context.Context, userID string, start, end time.Time) (tokens, costMicro int64, err error)

// Counter counts per-user usage per period in Redis and checks it against a
// user's Budgets.
type Counter struct {
	rdb    *redis.Client
	loc    *time.Location
	now    func() time.Time
	ledger LedgerSum
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

// unit is a counted quantity.
type unit int

const (
	unitTokens unit = iota
	unitCost
)

func (u unit) code() string {
	if u == unitTokens {
		return "tok"
	}
	return "cost"
}

const (
	// FreshFor is how long counters rebuilt from the ledger are trusted. A
	// request settled while its counters were being rebuilt, a failed
	// increment or a dropped ledger row can make a counter drift; after
	// FreshFor the next check re-derives it from the ledger (a soft
	// refresh: the old values keep being enforced until the new ones are
	// written), so any drift lasts about this long at most.
	FreshFor = 10 * time.Minute
	// counterGrace keeps counters a while past their period's end, for a
	// replica whose clock runs behind.
	counterGrace = 24 * time.Hour
	// rebuildLockTTL bounds how long one replica may hold the right to
	// rebuild a user's counters for a period; ledgerTimeout stays well
	// under it, so the lock never lapses during a rebuild.
	rebuildLockTTL = 30 * time.Second
)

// Variables so tests can shorten them.
var (
	ledgerTimeout = 15 * time.Second
	// waitMax and waitStep bound how long a check with no counters at all
	// waits for the replica rebuilding them before it lets the request
	// through.
	waitMax  = time.Second
	waitStep = 100 * time.Millisecond
	// unlockTimeout bounds releasing a rebuild lock.
	unlockTimeout = 2 * time.Second
)

// counterTTL is the TTL of counters written at now for a period ending at
// end. It is relative to the counter's own clock, not an absolute expiry, so
// a test clock set in the past does not create counters that vanish at once.
func counterTTL(now, end time.Time) time.Duration {
	return end.Sub(now) + counterGrace
}

// counterKey is the Redis key of one counter. It stays inside air:*, which
// is all the gateway's Redis ACL allows.
func counterKey(userID string, u unit, p Period, start time.Time) string {
	return "air:ub:" + userID + ":" + u.code() + ":" + p.code() + ":" + ID(p, start)
}

// lockKey is the key that lets one replica at a time rebuild a user's
// counters for one period (both units: one ledger read fills both).
func lockKey(userID string, p Period, start time.Time) string {
	return "air:ub:" + userID + ":lock:" + ID(p, start)
}

// freshKey marks a period's counters as recently derived from the ledger;
// it expires after FreshFor.
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

// errBusy marks a period another replica is rebuilding right now.
var errBusy = errors.New("budget: counters being rebuilt elsewhere")

// state is what Redis holds for one period.
type state struct {
	Usage
	have  bool // both counters exist
	fresh bool // the freshness marker exists
}

// load reads the counters and freshness markers of the periods ps at now.
func (c *Counter) load(ctx context.Context, userID string, now time.Time, ps []Period) (map[Period]state, error) {
	type cmds struct {
		tok, cost *redis.StringCmd
		fresh     *redis.IntCmd
	}
	pipe := c.rdb.Pipeline()
	all := make([]cmds, len(ps))
	for i, p := range ps {
		start, _ := Bounds(p, now, c.loc)
		all[i] = cmds{
			pipe.Get(ctx, counterKey(userID, unitTokens, p, start)),
			pipe.Get(ctx, counterKey(userID, unitCost, p, start)),
			pipe.Exists(ctx, freshKey(userID, p, start)),
		}
	}
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return nil, err
	}
	out := make(map[Period]state, len(ps))
	for i, p := range ps {
		tok, terr := all[i].tok.Int64()
		cost, cerr := all[i].cost.Int64()
		for _, err := range []error{terr, cerr} {
			if err != nil && !errors.Is(err, redis.Nil) {
				return nil, err
			}
		}
		out[p] = state{Usage: Usage{tok, cost}, have: terr == nil && cerr == nil, fresh: all[i].fresh.Val() == 1}
	}
	return out, nil
}

// read returns the usage of the periods ps at now.
//
// Existing counters are always what is enforced. When their freshness
// marker has expired, the replica that takes the period's rebuild lock
// re-derives them from the ledger first; the others keep using them as they
// are, so a refresh never opens a window without a check. Only when there
// are no counters at all (first use, or Redis lost them) does a check that
// did not get the lock wait, up to waitMax, for the replica rebuilding them;
// a period still without counters after that is absent from the result,
// and the caller lets the request through. The first error is returned
// alongside whatever could be read.
func (c *Counter) read(ctx context.Context, userID string, now time.Time, ps []Period) (map[Period]Usage, error) {
	states, err := c.load(ctx, userID, now, ps)
	if err != nil {
		return nil, err
	}
	out := make(map[Period]Usage, len(ps))
	var firstErr error
	keep := func(err error) {
		if err != nil && !errors.Is(err, errBusy) && firstErr == nil {
			firstErr = err
		}
	}
	var waiting []Period
	for _, p := range ps {
		st := states[p]
		if st.have {
			out[p] = st.Usage
			if !st.fresh {
				u, err := c.rebuild(ctx, userID, p, now)
				if err == nil {
					out[p] = u
				}
				keep(err)
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
	deadline := time.Now().Add(waitMax)
	for len(waiting) > 0 && time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			keep(ctx.Err())
			return out, firstErr
		case <-time.After(waitStep):
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
	return out, firstErr
}

// unlockScript deletes the lock only if it still holds this caller's token.
var unlockScript = redis.NewScript(`
if redis.call("get", KEYS[1]) == ARGV[1] then
	return redis.call("del", KEYS[1])
end
return 0
`)

// rebuild re-derives a period's counters from the ledger, overwriting them,
// and marks them fresh. Only the replica holding the period's rebuild lock
// does it; the others get errBusy, so a burst of requests costs one ledger
// read. It returns what the counters hold afterwards — a request settled
// since the write has been added — and if another replica finished a
// rebuild just before the lock was taken, those values without reading the
// ledger.
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
		uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), unlockTimeout)
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
	ttl := counterTTL(c.now(), end)
	pipe := c.rdb.Pipeline()
	pipe.Set(ctx, counterKey(userID, unitTokens, p, start), tok, ttl)
	pipe.Set(ctx, counterKey(userID, unitCost, p, start), cost, ttl)
	pipe.Set(ctx, freshKey(userID, p, start), 1, FreshFor)
	if _, err := pipe.Exec(ctx); err != nil {
		return Usage{}, err
	}
	if st, err := c.load(ctx, userID, now, []Period{p}); err == nil && st[p].have {
		return st[p].Usage, nil
	}
	return Usage{tok, cost}, nil
}

func randToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Check reports whether userID has reached any cap in b. Longer periods are
// checked first, so a denial names the cap that keeps the user blocked the
// longest. Counters missing from Redis are rebuilt from the ledger (see
// read). A period whose counters could not be read is not enforced for this
// request; on a Redis or ledger error the error is returned for the caller
// to log.
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
// from the same counters Check enforces.
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

// addScript increments each counter that already exists and leaves missing
// ones alone: a missing counter is rebuilt from the ledger, in full, by the
// next read, and creating it here would make it hold only this request.
// INCRBY keeps a key's TTL, so the counter still expires on schedule.
// ARGV[1] is the token delta, ARGV[2] the cost delta; KEYS alternate token
// and cost counters.
var addScript = redis.NewScript(`
for i, k in ipairs(KEYS) do
	local d = ARGV[2 - (i % 2)]
	if d ~= "0" and redis.call("exists", k) == 1 then
		redis.call("incrby", k, d)
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
	keys := make([]string, 0, 2*len(Periods))
	for _, p := range Periods {
		start, _ := Bounds(p, at, c.loc)
		keys = append(keys, counterKey(userID, unitTokens, p, start), counterKey(userID, unitCost, p, start))
	}
	return addScript.Run(ctx, c.rdb, keys, strconv.FormatInt(tokens, 10), strconv.FormatInt(costMicro, 10)).Err()
}
