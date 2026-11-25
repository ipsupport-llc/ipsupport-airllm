package budget

import (
	"context"
	"errors"
	"fmt"
	"strconv"
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

// counterTTLMargin keeps a counter a while past its period's end, so a
// replica whose clock runs a little behind still finds it. The TTL is
// relative to the counter's own clock, not an absolute expiry, so a test
// clock set in the past does not create counters that vanish at once.
const counterTTLMargin = 24 * time.Hour

// counterKey is the Redis key of one counter. It stays inside air:*, which
// is all the gateway's Redis ACL allows.
func counterKey(userID string, u unit, p Period, start time.Time) string {
	return "air:ub:" + userID + ":" + u.code() + ":" + p.code() + ":" + ID(p, start)
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
// $50.00 reached; resets 2026-12-01 00:00 UTC".
func (d Denial) Message() string {
	reset := d.ResetsAt.Format("2006-01-02 15:04") + " " + d.ResetsAt.Location().String()
	if d.Cost {
		return fmt.Sprintf("%s cost budget $%.2f reached; resets %s", d.Period.adjective(), float64(d.Limit)/1e6, reset)
	}
	return fmt.Sprintf("%s token budget %d reached; resets %s", d.Period.adjective(), d.Limit, reset)
}

// check is one counter a Check compares against a cap.
type check struct {
	unit  unit
	p     Period
	limit int64
	start time.Time
	end   time.Time
	key   string
}

// Check reports whether userID has reached any cap in b. Longer periods are
// checked first, so a denial names the cap that keeps the user blocked the
// longest. A counter missing from Redis is rebuilt from the ledger. On a
// Redis or ledger error it allows the request and returns the error for the
// caller to log.
func (c *Counter) Check(ctx context.Context, userID string, b Budgets) (Denial, bool, error) {
	if userID == "" || !b.Any() {
		return Denial{}, false, nil
	}
	now := c.now()
	var checks []check
	for i := len(Periods) - 1; i >= 0; i-- {
		p := Periods[i]
		start, end := Bounds(p, now, c.loc)
		if lim := b.TokenCap(p); lim > 0 {
			checks = append(checks, check{unitTokens, p, lim, start, end, counterKey(userID, unitTokens, p, start)})
		}
		if lim := b.CostCapMicro(p); lim > 0 {
			checks = append(checks, check{unitCost, p, lim, start, end, counterKey(userID, unitCost, p, start)})
		}
	}

	pipe := c.rdb.Pipeline()
	gets := make([]*redis.StringCmd, len(checks))
	for i, ch := range checks {
		gets[i] = pipe.Get(ctx, ch.key)
	}
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return Denial{}, false, err
	}
	used := make([]int64, len(checks))
	var firstErr error
	rebuilt := map[Period][2]int64{} // one ledger read per period, both units
	for i, ch := range checks {
		v, err := gets[i].Int64()
		if err == nil {
			used[i] = v
			continue
		}
		if !errors.Is(err, redis.Nil) {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		sums, ok := rebuilt[ch.p]
		if !ok {
			tok, cost, err := c.ledger(ctx, userID, ch.start, ch.end)
			if err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("budget: rebuild from ledger: %w", err)
				}
				continue
			}
			sums = [2]int64{tok, cost}
			rebuilt[ch.p] = sums
		}
		val := sums[ch.unit]
		// SET NX: a replica that rebuilt the same counter first wins, and
		// whatever it holds now is the value to compare against.
		_, err = c.rdb.SetArgs(ctx, ch.key, val, redis.SetArgs{Mode: "NX", TTL: ch.end.Sub(now) + counterTTLMargin}).Result()
		switch {
		case err == nil: // we created it
		case errors.Is(err, redis.Nil):
			if n, gerr := c.rdb.Get(ctx, ch.key).Int64(); gerr == nil {
				val = n
			}
		default:
			if firstErr == nil {
				firstErr = err
			}
		}
		used[i] = val
	}
	for i, ch := range checks {
		if used[i] >= ch.limit {
			return Denial{Period: ch.p, Cost: ch.unit == unitCost, Limit: ch.limit, Used: used[i], ResetsAt: ch.end}, true, firstErr
		}
	}
	return Denial{}, false, firstErr
}

// addScript increments each counter that already exists and leaves missing
// ones alone: a missing counter is rebuilt from the ledger, in full, by the
// next Check, and creating it here would make it hold only this request.
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

// Add records a settled request's usage against every period's counters
// that exist.
func (c *Counter) Add(ctx context.Context, userID string, tokens, costMicro int64) error {
	if userID == "" || (tokens == 0 && costMicro == 0) {
		return nil
	}
	now := c.now()
	keys := make([]string, 0, 2*len(Periods))
	for _, p := range Periods {
		start, _ := Bounds(p, now, c.loc)
		keys = append(keys, counterKey(userID, unitTokens, p, start), counterKey(userID, unitCost, p, start))
	}
	return addScript.Run(ctx, c.rdb, keys, strconv.FormatInt(tokens, 10), strconv.FormatInt(costMicro, 10)).Err()
}
