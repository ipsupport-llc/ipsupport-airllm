// Package limits enforces per-key rolling-window usage caps using Redis time
// buckets. Usage is accumulated into fixed-size buckets (per key, per unit);
// a window sum adds the buckets falling inside [now-window, now]. Enforcement
// is check-before / increment-after.
package limits

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/policy"
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
	Unit    string // "tokens" | "cost_usd" | "audio_seconds" | "tts_chars"
	Limit   int64  // tokens, micro-USD, audio seconds, or TTS characters
	Used    int64

	// ReservedTokens/ReservedBucket describe an optimistic token-headroom
	// reservation Check made into the bucket when it allowed the request
	// (see Check's doc). Zero means nothing was reserved. The caller MUST
	// pass both through to the matching Add call — even for a failed or
	// zero-usage request — so Add's correction can refund the reservation.
	ReservedTokens int64
	ReservedBucket int64
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

func tokKey(key string) string      { return "air:u:" + key + ":tok" }
func costKey(key string) string     { return "air:u:" + key + ":cost" }
func audioSecKey(key string) string { return "air:u:" + key + ":asec" }
func ttsCharKey(key string) string  { return "air:u:" + key + ":ttsc" }

// BucketStamp returns the bucket timestamp (unix seconds) for t.
func BucketStamp(t time.Time) int64 {
	sz := int64(BucketSize.Seconds())
	return t.Unix() / sz * sz
}

// Check reports whether a request is allowed under lim, given current usage.
// On a backend error it fails open (allow) so a Redis outage cannot take the
// gateway down; the caller logs the error.
//
// When lim has a token limit configured and reserveTokens > 0, an allowed
// decision ALSO atomically reserves reserveTokens of headroom into the
// current bucket. This closes the check-then-increment race: completion
// token counts aren't known until the response finishes, so the matching
// Add call runs long after Check — without a reservation, nothing stops a
// second concurrent Check for the same key from reading the same
// pre-increment state and also being allowed. The reservation is a
// conservative estimate (the caller's declared max_tokens, or a default
// ceiling) that the matching Add call MUST correct to the real usage via
// Decision.ReservedTokens/ReservedBucket, even when the request fails or
// used zero tokens (otherwise the reservation is never refunded). Only the
// primary tokens dimension is reserved this way — cost_usd/audio_seconds/
// tts_chars quantities aren't knowable at Check time anywhere in this
// codebase's call sites, so they remain subject to the same class of race;
// a narrower, documented residual rather than a silently unfixed Critical.
func (l *Limiter) Check(ctx context.Context, key string, lim policy.Limits, reserveTokens int64) (Decision, error) {
	if len(lim.Tokens) == 0 && len(lim.CostUSD) == 0 && len(lim.AudioSeconds) == 0 && len(lim.TTSChars) == 0 {
		return Decision{Allowed: true}, nil
	}

	reserve := reserveTokens > 0 && len(lim.Tokens) > 0
	if reserve {
		unlock, err := l.lockKey(ctx, key)
		if err != nil {
			// Couldn't get exclusive access to this key within the retry
			// budget; fail open on the RESERVATION only (same philosophy as
			// any other Redis trouble below) rather than block the request.
			reserve = false
		} else {
			defer unlock()
		}
	}

	now := l.now()
	pipe := l.rdb.Pipeline()
	tokCmd := pipe.HGetAll(ctx, tokKey(key))
	costCmd := pipe.HGetAll(ctx, costKey(key))
	audioCmd := pipe.HGetAll(ctx, audioSecKey(key))
	ttsCmd := pipe.HGetAll(ctx, ttsCharKey(key))
	if _, err := pipe.Exec(ctx); err != nil {
		return Decision{Allowed: true}, err
	}
	tokFields := tokCmd.Val()
	costFields := costCmd.Val()
	audioFields := audioCmd.Val()
	ttsFields := ttsCmd.Val()

	l.prune(ctx, key, now, tokFields, costFields, audioFields, ttsFields)

	tokSums := SumWindows(now, tokFields)
	costSums := SumWindows(now, costFields)
	audioSums := SumWindows(now, audioFields)
	ttsSums := SumWindows(now, ttsFields)

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
		if max, ok := lim.AudioSeconds[win.Name]; ok && max > 0 && audioSums[win.Name] >= max {
			return Decision{Allowed: false, Window: win.Name, Unit: "audio_seconds", Limit: max, Used: audioSums[win.Name]}, nil
		}
		if max, ok := lim.TTSChars[win.Name]; ok && max > 0 && ttsSums[win.Name] >= max {
			return Decision{Allowed: false, Window: win.Name, Unit: "tts_chars", Limit: max, Used: ttsSums[win.Name]}, nil
		}
	}

	dec := Decision{Allowed: true}
	if reserve {
		bucket := BucketStamp(now)
		if err := l.rdb.HIncrBy(ctx, tokKey(key), strconv.FormatInt(bucket, 10), reserveTokens).Err(); err == nil {
			dec.ReservedTokens = reserveTokens
			dec.ReservedBucket = bucket
		}
	}
	return dec, nil
}

const (
	lockTTL        = 2 * time.Second
	lockRetries    = 5
	lockRetryDelay = 20 * time.Millisecond
)

// unlockScript deletes the lock only if it still holds the token this call
// set — a compare-and-delete, so a caller whose lock already expired (e.g.
// a slow request) can't release a DIFFERENT caller's lock on the same key.
var unlockScript = redis.NewScript(`
if redis.call("get", KEYS[1]) == ARGV[1] then
	return redis.call("del", KEYS[1])
else
	return 0
end
`)

// lockKey acquires a short-lived, per-key distributed lock (via Redis, so it
// serializes across processes/pods, not just goroutines) so Check's
// read-then-reserve sequence can't interleave with another Check for the
// same key. Returns an unlock function; callers must defer it.
func (l *Limiter) lockKey(ctx context.Context, key string) (func(), error) {
	name := "air:lock:" + key
	token := randToken()
	for i := 0; i < lockRetries; i++ {
		ok, err := l.rdb.SetNX(ctx, name, token, lockTTL).Result()
		if err != nil {
			return nil, err
		}
		if ok {
			return func() {
				_ = unlockScript.Run(context.Background(), l.rdb, []string{name}, token).Err()
			}, nil
		}
		time.Sleep(lockRetryDelay)
	}
	return nil, errors.New("limits: could not acquire key lock")
}

func randToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Add increments the current bucket with the given usage and refreshes the
// hash TTLs so idle keys eventually expire. Any zero-valued quantity is
// skipped (no Redis write for a dimension a request didn't use).
//
// reservedTokens/reservedBucket, when non-zero, come from the Check call
// that preceded this request (see Check's doc) — the tokens dimension is
// then corrected by (tokens - reservedTokens) written into reservedBucket
// specifically (not necessarily "now"; the request may have crossed a
// bucket boundary while running), netting the reservation down to the real
// usage. This correction runs even when tokens == 0, so a failed or
// zero-usage request still refunds its reservation instead of leaking it.
func (l *Limiter) Add(ctx context.Context, key string, tokens, costMicroUSD, audioSeconds, ttsChars, reservedTokens, reservedBucket int64) error {
	nowField := strconv.FormatInt(BucketStamp(l.now()), 10)
	tokField := nowField
	tokDelta := tokens
	if reservedTokens != 0 {
		tokDelta = tokens - reservedTokens
		tokField = strconv.FormatInt(reservedBucket, 10)
	}
	if tokDelta == 0 && costMicroUSD == 0 && audioSeconds == 0 && ttsChars == 0 {
		return nil
	}
	ttl := maxWindow() + 2*BucketSize

	pipe := l.rdb.Pipeline()
	if tokDelta != 0 {
		pipe.HIncrBy(ctx, tokKey(key), tokField, tokDelta)
		pipe.Expire(ctx, tokKey(key), ttl)
	}
	if costMicroUSD != 0 {
		pipe.HIncrBy(ctx, costKey(key), nowField, costMicroUSD)
		pipe.Expire(ctx, costKey(key), ttl)
	}
	if audioSeconds != 0 {
		pipe.HIncrBy(ctx, audioSecKey(key), nowField, audioSeconds)
		pipe.Expire(ctx, audioSecKey(key), ttl)
	}
	if ttsChars != 0 {
		pipe.HIncrBy(ctx, ttsCharKey(key), nowField, ttsChars)
		pipe.Expire(ctx, ttsCharKey(key), ttl)
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

// prune deletes buckets older than the longest window from all dimension hashes.
func (l *Limiter) prune(ctx context.Context, key string, now time.Time, tokFields, costFields, audioFields, ttsFields map[string]string) {
	cutoff := now.Add(-maxWindow() - BucketSize).Unix()
	tokExpired := expiredFields(cutoff, tokFields)
	costExpired := expiredFields(cutoff, costFields)
	audioExpired := expiredFields(cutoff, audioFields)
	ttsExpired := expiredFields(cutoff, ttsFields)
	if len(tokExpired) == 0 && len(costExpired) == 0 && len(audioExpired) == 0 && len(ttsExpired) == 0 {
		return
	}
	pipe := l.rdb.Pipeline()
	if len(tokExpired) > 0 {
		pipe.HDel(ctx, tokKey(key), tokExpired...)
	}
	if len(costExpired) > 0 {
		pipe.HDel(ctx, costKey(key), costExpired...)
	}
	if len(audioExpired) > 0 {
		pipe.HDel(ctx, audioSecKey(key), audioExpired...)
	}
	if len(ttsExpired) > 0 {
		pipe.HDel(ctx, ttsCharKey(key), ttsExpired...)
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
