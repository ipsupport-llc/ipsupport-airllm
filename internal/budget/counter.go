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
	// CounterTTL is how long a counter lives once rebuilt from the ledger.
	// Counters are a short-lived cache of the ledger: a request settled
	// while its counter was being rebuilt, a failed increment or a dropped
	// ledger row can make one drift, and expiring it re-derives it from the
	// ledger, so any drift lasts at most this long.
	CounterTTL = 10 * time.Minute
	// counterGrace keeps a counter a little past its period's end, for a
	// replica whose clock runs slightly behind.
	counterGrace = time.Minute
	// rebuildLockTTL bounds how long one replica may hold the right to
	// rebuild a user's counters for a period.
	rebuildLockTTL = 5 * time.Second
)

// counterTTL is the TTL of a counter rebuilt at now for a period ending at
// end. It is relative to the counter's own clock, not an absolute expiry, so
// a test clock set in the past does not create counters that vanish at once.
func counterTTL(now, end time.Time) time.Duration {
	return min(CounterTTL, end.Sub(now)+counterGrace)
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

// read returns the counters of the periods ps at now, rebuilding missing
// ones from the ledger. A period another replica is rebuilding at this
// moment, or whose read failed, is absent from the result; the first
// error other than that contention is returned alongside.
func (c *Counter) read(ctx context.Context, userID string, now time.Time, ps []Period) (map[Period]Usage, error) {
	pipe := c.rdb.Pipeline()
	type pair struct{ tok, cost *redis.StringCmd }
	gets := make([]pair, len(ps))
	for i, p := range ps {
		start, _ := Bounds(p, now, c.loc)
		gets[i] = pair{pipe.Get(ctx, counterKey(userID, unitTokens, p, start)), pipe.Get(ctx, counterKey(userID, unitCost, p, start))}
	}
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return nil, err
	}
	out := make(map[Period]Usage, len(ps))
	var firstErr error
	for i, p := range ps {
		tok, terr := gets[i].tok.Int64()
		cost, cerr := gets[i].cost.Int64()
		if terr == nil && cerr == nil {
			out[p] = Usage{tok, cost}
			continue
		}
		if (terr != nil && !errors.Is(terr, redis.Nil)) || (cerr != nil && !errors.Is(cerr, redis.Nil)) {
			if firstErr == nil {
				firstErr = errors.Join(terr, cerr)
			}
			continue
		}
		u, err := c.rebuild(ctx, userID, now, p, terr == nil, tok, cerr == nil, cost)
		if err != nil {
			if !errors.Is(err, errBusy) && firstErr == nil {
				firstErr = err
			}
			continue
		}
		out[p] = u
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

// rebuild recreates a period's missing counters from the ledger. Only the
// replica that takes the period's rebuild lock reads the ledger; the others
// get errBusy, so a burst of requests at the start of a period (or when the
// counters expire) costs one ledger read. A counter that still exists keeps
// its value (haveTok/haveCost).
func (c *Counter) rebuild(ctx context.Context, userID string, now time.Time, p Period, haveTok bool, tok int64, haveCost bool, cost int64) (Usage, error) {
	start, end := Bounds(p, now, c.loc)
	lock, token := lockKey(userID, p, start), randToken()
	ok, err := c.rdb.SetNX(ctx, lock, token, rebuildLockTTL).Result()
	if err != nil {
		return Usage{}, err
	}
	if !ok {
		return Usage{}, errBusy
	}
	defer func() { _ = unlockScript.Run(context.WithoutCancel(ctx), c.rdb, []string{lock}, token).Err() }()

	ltok, lcost, err := c.ledger(ctx, userID, start, end)
	if err != nil {
		return Usage{}, fmt.Errorf("budget: rebuild from ledger: %w", err)
	}
	ttl := counterTTL(now, end)
	pipe := c.rdb.Pipeline()
	if !haveTok {
		tok = ltok
		pipe.SetNX(ctx, counterKey(userID, unitTokens, p, start), tok, ttl)
	}
	if !haveCost {
		cost = lcost
		pipe.SetNX(ctx, counterKey(userID, unitCost, p, start), cost, ttl)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return Usage{}, err
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
// longest. A counter missing from Redis is rebuilt from the ledger. A period
// whose counters another replica is rebuilding at this instant is not
// enforced for this request (fail open, for the moment the rebuild takes).
// On a Redis or ledger error it allows the request and returns the error for
// the caller to log.
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

// Usage returns the user's usage in the current day, week and month from the
// counters (rebuilding missing ones). A period another replica is
// rebuilding at this instant is read from the ledger directly instead.
func (c *Counter) Usage(ctx context.Context, userID string) (map[Period]Usage, error) {
	now := c.now()
	used, err := c.read(ctx, userID, now, Periods)
	if err != nil {
		return nil, err
	}
	for _, p := range Periods {
		if _, ok := used[p]; ok {
			continue
		}
		start, end := Bounds(p, now, c.loc)
		tok, cost, err := c.ledger(ctx, userID, start, end)
		if err != nil {
			return nil, err
		}
		used[p] = Usage{tok, cost}
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
