// Package limits enforces per-key rolling-window usage caps using Redis time
// buckets. Usage is accumulated into fixed-size buckets (per key, per unit);
// a window sum adds the buckets falling inside [now-window, now]. Enforcement
// is check-before / increment-after.
package limits

import (
	"context"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/rromenskyi/ipsupport-airouter/internal/policy"
)

// BucketSize is the granularity of usage buckets; window edges are accurate
// to within one bucket.
const BucketSize = 5 * time.Minute

// Window is a named rolling window.
type Window struct {
	Name string
	Dur  time.Duration
}

// Windows are the enforced rolling windows, longest last.
var Windows = []Window{
	{"5h", 5 * time.Hour},
	{"24h", 24 * time.Hour},
	{"7d", 7 * 24 * time.Hour},
}

func maxWindow() time.Duration { return Windows[len(Windows)-1].Dur }

// Decision is the outcome of a limit check.
type Decision struct {
	Allowed bool
	Window  string
	Unit    string // "tokens" | "cost_usd"
	Limit   int64  // tokens, or micro-USD
	Used    int64
}

// Limiter checks and records usage against Redis.
type Limiter struct {
	rdb *redis.Client
	now func() time.Time
}

// New returns a Limiter using the wall clock.
func New(rdb *redis.Client) *Limiter {
	return &Limiter{rdb: rdb, now: time.Now}
}

func tokKey(key string) string  { return "air:u:" + key + ":tok" }
func costKey(key string) string { return "air:u:" + key + ":cost" }

// BucketStamp returns the bucket timestamp (unix seconds) for t.
func BucketStamp(t time.Time) int64 {
	sz := int64(BucketSize.Seconds())
	return t.Unix() / sz * sz
}

// Check reports whether a request is allowed under lim, given current usage.
// On a backend error it fails open (allow) so a Redis outage cannot take the
// gateway down; the caller logs the error.
func (l *Limiter) Check(ctx context.Context, key string, lim policy.Limits) (Decision, error) {
	if len(lim.Tokens) == 0 && len(lim.CostUSD) == 0 {
		return Decision{Allowed: true}, nil
	}

	now := l.now()
	tokFields, err := l.rdb.HGetAll(ctx, tokKey(key)).Result()
	if err != nil {
		return Decision{Allowed: true}, err
	}
	costFields, err := l.rdb.HGetAll(ctx, costKey(key)).Result()
	if err != nil {
		return Decision{Allowed: true}, err
	}

	l.prune(ctx, key, now, tokFields, costFields)

	tokSums := SumWindows(now, tokFields)
	costSums := SumWindows(now, costFields)

	for _, win := range Windows {
		if max, ok := lim.Tokens[win.Name]; ok && max > 0 && tokSums[win.Name] >= max {
			return Decision{Allowed: false, Window: win.Name, Unit: "tokens", Limit: max, Used: tokSums[win.Name]}, nil
		}
		if usd, ok := lim.CostUSD[win.Name]; ok && usd > 0 {
			maxMicro := int64(usd * 1e6)
			if costSums[win.Name] >= maxMicro {
				return Decision{Allowed: false, Window: win.Name, Unit: "cost_usd", Limit: maxMicro, Used: costSums[win.Name]}, nil
			}
		}
	}
	return Decision{Allowed: true}, nil
}

// Add increments the current bucket with the given usage and refreshes the
// hash TTLs so idle keys eventually expire.
func (l *Limiter) Add(ctx context.Context, key string, tokens, costMicroUSD int64) error {
	if tokens == 0 && costMicroUSD == 0 {
		return nil
	}
	field := strconv.FormatInt(BucketStamp(l.now()), 10)
	ttl := maxWindow() + 2*BucketSize

	pipe := l.rdb.Pipeline()
	if tokens != 0 {
		pipe.HIncrBy(ctx, tokKey(key), field, tokens)
		pipe.Expire(ctx, tokKey(key), ttl)
	}
	if costMicroUSD != 0 {
		pipe.HIncrBy(ctx, costKey(key), field, costMicroUSD)
		pipe.Expire(ctx, costKey(key), ttl)
	}
	_, err := pipe.Exec(ctx)
	return err
}

// SumWindows sums bucket fields into a per-window total.
func SumWindows(now time.Time, fields map[string]string) map[string]int64 {
	out := make(map[string]int64, len(Windows))
	for _, win := range Windows {
		cutoff := now.Add(-win.Dur).Unix()
		var sum int64
		for f, v := range fields {
			ts, err := strconv.ParseInt(f, 10, 64)
			if err != nil || ts < cutoff {
				continue
			}
			n, err := strconv.ParseInt(v, 10, 64)
			if err == nil {
				sum += n
			}
		}
		out[win.Name] = sum
	}
	return out
}

// prune deletes buckets older than the longest window from both hashes.
func (l *Limiter) prune(ctx context.Context, key string, now time.Time, tokFields, costFields map[string]string) {
	cutoff := now.Add(-maxWindow() - BucketSize).Unix()
	tokExpired := expiredFields(cutoff, tokFields)
	costExpired := expiredFields(cutoff, costFields)
	if len(tokExpired) == 0 && len(costExpired) == 0 {
		return
	}
	pipe := l.rdb.Pipeline()
	if len(tokExpired) > 0 {
		pipe.HDel(ctx, tokKey(key), tokExpired...)
	}
	if len(costExpired) > 0 {
		pipe.HDel(ctx, costKey(key), costExpired...)
	}
	_, _ = pipe.Exec(ctx)
}

func expiredFields(cutoff int64, fields map[string]string) []string {
	var out []string
	for f := range fields {
		ts, err := strconv.ParseInt(f, 10, 64)
		if err != nil || ts < cutoff {
			out = append(out, f)
		}
	}
	return out
}
