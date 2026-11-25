package budget

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// testRedis connects to TEST_REDIS_URL or skips.
func testRedis(t *testing.T) *redis.Client {
	t.Helper()
	dsn := os.Getenv("TEST_REDIS_URL")
	if dsn == "" {
		t.Skip("TEST_REDIS_URL not set; skipping budget counter integration test")
	}
	opt, err := redis.ParseURL(dsn)
	if err != nil {
		t.Fatalf("parse redis url: %v", err)
	}
	rdb := redis.NewClient(opt)
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

// fakeLedger answers ledger sums from fixed rows and records each range it
// was asked for. Tests change its fields only while no read is running.
type fakeLedger struct {
	mu    sync.Mutex
	rows  []ledgerRow
	asked [][2]time.Time
	err   error
	block bool          // each sum waits for its context to end
	gate  chan struct{} // when set, each sum waits for it to close
}

type ledgerRow struct {
	ts         time.Time
	tokens     int64
	costMicros int64
}

func (f *fakeLedger) sum(ctx context.Context, _ string, start, end time.Time) (int64, int64, error) {
	f.mu.Lock()
	f.asked = append(f.asked, [2]time.Time{start, end})
	block, gate := f.block, f.gate
	f.mu.Unlock()
	if block {
		<-ctx.Done()
		return 0, 0, ctx.Err()
	}
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return 0, 0, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return 0, 0, f.err
	}
	var tok, cost int64
	for _, r := range f.rows {
		if !r.ts.Before(start) && r.ts.Before(end) {
			tok += r.tokens
			cost += r.costMicros
		}
	}
	return tok, cost, nil
}

func (f *fakeLedger) asks() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.asked)
}

func newTestCounter(t *testing.T, loc *time.Location, at time.Time) (*Counter, *clock, *fakeLedger, string) {
	rdb := testRedis(t)
	clk := &clock{t: at}
	led := &fakeLedger{}
	user := fmt.Sprintf("budget-test-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		ctx := context.Background()
		iter := rdb.Scan(ctx, 0, "air:ub:"+user+"*", 100).Iterator()
		for iter.Next(ctx) {
			_ = rdb.Del(ctx, iter.Val()).Err()
		}
	})
	c := NewCounter(rdb, loc, clk.now, led.sum)
	t.Cleanup(c.bg.Wait) // runs before the keys are removed
	return c, clk, led, user
}

// shorten sets a tunable for the rest of the test.
func shorten(t *testing.T, v *time.Duration, d time.Duration) {
	old := *v
	*v = d
	t.Cleanup(func() { *v = old })
}

func mustCheck(t *testing.T, c *Counter, user string, b Budgets, wantDenied bool, when string) Denial {
	t.Helper()
	d, denied, err := c.Check(context.Background(), user, b)
	if err != nil {
		t.Fatalf("%s: check: %v", when, err)
	}
	if denied != wantDenied {
		t.Fatalf("%s: denied=%v (%+v), want %v", when, denied, d, wantDenied)
	}
	return d
}

func mustAdd(t *testing.T, c *Counter, user string, tokens, cost int64) {
	t.Helper()
	if err := c.Add(context.Background(), user, c.now(), tokens, cost); err != nil {
		t.Fatalf("add: %v", err)
	}
}

// TestCheckRebuildsAMissingCounterFromTheLedger: with no counter in Redis,
// the ledger's usage for the period is what the check compares, and the
// rebuilt counter is then incremented in place rather than read again.
func TestCheckRebuildsAMissingCounterFromTheLedger(t *testing.T) {
	at := time.Date(2026, 11, 24, 15, 0, 0, 0, time.UTC)
	c, _, led, user := newTestCounter(t, time.UTC, at)
	led.rows = []ledgerRow{
		{ts: time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC), costMicros: 30_000_000},
		{ts: time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC), costMicros: 15_000_000},
		{ts: time.Date(2026, 10, 31, 23, 0, 0, 0, time.UTC), costMicros: 99_000_000}, // last month
	}
	b := Budgets{CostUSD: CostCaps{Month: 50}}
	mustCheck(t, c, user, b, false, "45 of 50 used")
	if len(led.asked) != 1 || !led.asked[0][0].Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("ledger asked for %v, want the month from 1 Nov", led.asked)
	}
	mustAdd(t, c, user, 0, 5_000_000)
	d := mustCheck(t, c, user, b, true, "50 of 50 used")
	if len(led.asked) != 1 {
		t.Errorf("ledger read again (%d times) although the counter exists", len(led.asked))
	}
	if got, want := d.Message(), "monthly cost budget $50.00 reached; resets 2026-12-01 00:00 UTC"; got != want {
		t.Errorf("message %q, want %q", got, want)
	}
}

// TestAddDoesNotCreateCounters: settling a request before any check leaves
// the counter missing, so the next check still rebuilds the full period.
func TestAddDoesNotCreateCounters(t *testing.T) {
	at := time.Date(2026, 11, 24, 15, 0, 0, 0, time.UTC)
	c, _, led, user := newTestCounter(t, time.UTC, at)
	mustAdd(t, c, user, 10, 0)
	led.rows = []ledgerRow{{ts: at.Add(-time.Hour), tokens: 1000}}
	mustCheck(t, c, user, Budgets{Tokens: TokenCaps{Day: 1000}}, true, "ledger holds the full day")
}

func TestLedgerErrorFailsOpen(t *testing.T) {
	at := time.Date(2026, 11, 24, 15, 0, 0, 0, time.UTC)
	c, _, led, user := newTestCounter(t, time.UTC, at)
	led.err = errors.New("db down")
	_, denied, err := c.Check(context.Background(), user, Budgets{Tokens: TokenCaps{Day: 1}})
	if denied || err == nil {
		t.Fatalf("denied=%v err=%v, want allowed with the error", denied, err)
	}
	led.err = nil
	led.rows = []ledgerRow{{ts: at, tokens: 5}}
	mustCheck(t, c, user, Budgets{Tokens: TokenCaps{Day: 1}}, true, "after the ledger recovered (nothing was cached)")
}

func TestRedisErrorFailsOpen(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	c := NewCounter(rdb, nil, nil, (&fakeLedger{}).sum)
	_, denied, err := c.Check(context.Background(), "u", Budgets{Tokens: TokenCaps{Day: 1}})
	if denied || err == nil {
		t.Fatalf("denied=%v err=%v, want allowed with the error", denied, err)
	}
}

func TestNoCapsAsksNothing(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	led := &fakeLedger{}
	c := NewCounter(rdb, nil, nil, led.sum)
	if _, denied, err := c.Check(context.Background(), "u", Budgets{}); denied || err != nil {
		t.Fatalf("denied=%v err=%v, want allowed without touching Redis", denied, err)
	}
	if _, denied, err := c.Check(context.Background(), "", Budgets{Tokens: TokenCaps{Day: 1}}); denied || err != nil {
		t.Fatalf("no user: denied=%v err=%v, want allowed", denied, err)
	}
}

// TestDayRollsOverAtLocalMidnight: a daily cap reached late in the day stops
// applying at midnight in the configured zone, not at UTC midnight.
func TestDayRollsOverAtLocalMidnight(t *testing.T) {
	berlin, _ := time.LoadLocation("Europe/Berlin")
	c, clk, _, user := newTestCounter(t, berlin, time.Date(2026, 11, 24, 23, 50, 0, 0, berlin))
	b := Budgets{Tokens: TokenCaps{Day: 100}}
	mustCheck(t, c, user, b, false, "fresh day")
	mustAdd(t, c, user, 100, 0)
	d := mustCheck(t, c, user, b, true, "cap reached at 23:50")
	if want := time.Date(2026, 11, 25, 0, 0, 0, 0, berlin); !d.ResetsAt.Equal(want) {
		t.Errorf("resets at %v, want %v", d.ResetsAt, want)
	}
	// 00:10 in Berlin is still 23:10 UTC on the 24th.
	clk.set(time.Date(2026, 11, 25, 0, 10, 0, 0, berlin))
	mustCheck(t, c, user, b, false, "after local midnight")
}

// TestWeekRollsOverAcrossTheISOYearBoundary: the last ISO week of 2026 runs
// from Mon 28 Dec 2026 to Sun 3 Jan 2027; a cap reached on New Year's Day is
// still in force on 3 Jan and lifted on Monday 4 Jan.
func TestWeekRollsOverAcrossTheISOYearBoundary(t *testing.T) {
	c, clk, _, user := newTestCounter(t, time.UTC, time.Date(2026, 12, 30, 10, 0, 0, 0, time.UTC))
	b := Budgets{Tokens: TokenCaps{Week: 500}}
	mustCheck(t, c, user, b, false, "start of the week")
	mustAdd(t, c, user, 300, 0)
	clk.set(time.Date(2027, 1, 1, 10, 0, 0, 0, time.UTC))
	mustAdd(t, c, user, 200, 0)
	d := mustCheck(t, c, user, b, true, "new year's day, same ISO week")
	if want := time.Date(2027, 1, 4, 0, 0, 0, 0, time.UTC); !d.ResetsAt.Equal(want) {
		t.Errorf("resets at %v, want %v", d.ResetsAt, want)
	}
	clk.set(time.Date(2027, 1, 3, 23, 59, 0, 0, time.UTC))
	mustCheck(t, c, user, b, true, "Sunday 3 Jan, still 2026-W53")
	clk.set(time.Date(2027, 1, 4, 0, 0, 0, 0, time.UTC))
	mustCheck(t, c, user, b, false, "Monday 4 Jan, 2027-W01")
}

// TestDayAcrossADSTChange: New York falls back on 1 Nov 2026, so that day
// lasts 25 hours; the cap holds through the repeated hour and lifts at the
// next local midnight.
func TestDayAcrossADSTChange(t *testing.T) {
	ny, _ := time.LoadLocation("America/New_York")
	c, clk, _, user := newTestCounter(t, ny, time.Date(2026, 11, 1, 0, 30, 0, 0, ny))
	b := Budgets{CostUSD: CostCaps{Day: 1}}
	mustCheck(t, c, user, b, false, "start of the day")
	mustAdd(t, c, user, 0, 1_000_000)
	// 24 hours after 00:30 EDT it is 23:30 EST on the same date.
	clk.set(time.Date(2026, 11, 1, 0, 30, 0, 0, ny).Add(24 * time.Hour))
	if got := clk.now().In(ny).Day(); got != 1 {
		t.Fatalf("24h later is day %d, want still the 1st", got)
	}
	d := mustCheck(t, c, user, b, true, "24h later, still 1 Nov local")
	if want := time.Date(2026, 11, 2, 0, 0, 0, 0, ny); !d.ResetsAt.Equal(want) {
		t.Errorf("resets at %v, want %v", d.ResetsAt, want)
	}
	clk.set(time.Date(2026, 11, 2, 0, 0, 1, 0, ny))
	mustCheck(t, c, user, b, false, "after local midnight")
}

// TestLongestPeriodIsReported: with both the day and the month over their
// caps, the denial names the month — the one that keeps blocking.
func TestLongestPeriodIsReported(t *testing.T) {
	at := time.Date(2026, 11, 24, 15, 0, 0, 0, time.UTC)
	c, _, led, user := newTestCounter(t, time.UTC, at)
	led.rows = []ledgerRow{{ts: at, tokens: 100, costMicros: 10_000_000}}
	d := mustCheck(t, c, user, Budgets{Tokens: TokenCaps{Day: 50}, CostUSD: CostCaps{Month: 10}}, true, "both over")
	if d.Period != Month || !d.Cost {
		t.Errorf("denial %+v, want the monthly cost cap", d)
	}
}

var (
	nov24  = time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC)
	nov24h = time.Date(2026, 11, 24, 15, 0, 0, 0, time.UTC)
)

func dayHash(user string) string { return hashKey(user, Day, nov24) }

// TestCountersLiveUntilThePeriodEnds: a built day hash expires a day after
// the day ends; its freshness marker after FreshFor.
func TestCountersLiveUntilThePeriodEnds(t *testing.T) {
	c, _, _, user := newTestCounter(t, time.UTC, nov24h)
	ctx := context.Background()
	mustCheck(t, c, user, Budgets{Tokens: TokenCaps{Day: 100}}, false, "build")
	want := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC).Sub(nov24h) + counterGrace // 33h
	if ttl, err := c.rdb.PTTL(ctx, dayHash(user)).Result(); err != nil || ttl > want || ttl < want-time.Minute {
		t.Errorf("day hash TTL = %v (%v), want about %v", ttl, err, want)
	}
	if ttl, err := c.rdb.PTTL(ctx, freshKey(user, Day, nov24)).Result(); err != nil || ttl > FreshFor || ttl < FreshFor-time.Minute {
		t.Errorf("fresh marker TTL = %v (%v), want about %v", ttl, err, FreshFor)
	}
	if got := counterTTL(time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC), nov24); got != minCounterTTL {
		t.Errorf("TTL past the period's grace = %v, want the floor %v", got, minCounterTTL)
	}
}

// TestStaleCountersAreRefreshedInTheBackground: once the freshness marker
// is gone, checks keep enforcing the existing counters at once — none waits
// for the ledger — while one background refresh re-derives them.
func TestStaleCountersAreRefreshedInTheBackground(t *testing.T) {
	c, _, led, user := newTestCounter(t, time.UTC, nov24h)
	ctx := context.Background()
	b := Budgets{Tokens: TokenCaps{Day: 100}}
	mustCheck(t, c, user, b, false, "build from an empty ledger")

	led.rows = []ledgerRow{{ts: nov24h, tokens: 100}} // never settled: the counter drifted
	led.gate = make(chan struct{})
	_ = c.rdb.Del(ctx, freshKey(user, Day, nov24)).Err()
	asked := led.asks()
	for i := 0; i < 10; i++ {
		mustCheck(t, c, user, b, false, "stale counter, refresh blocked in the ledger")
	}
	close(led.gate)
	c.bg.Wait()
	if n := led.asks() - asked; n != 1 {
		t.Errorf("10 stale checks started %d ledger reads, want 1", n)
	}
	mustCheck(t, c, user, b, true, "refreshed")
	if v, _ := c.rdb.HGet(ctx, dayHash(user), "tok").Int64(); v != 100 {
		t.Errorf("tok = %d after the refresh, want the ledger's 100", v)
	}
}

// TestRefreshKeepsIncrementsThatLandDuringIt: a request settled after the
// refresh read the ledger (its ledger row not yet written) stays counted —
// the refresh publishes max(counter, ledger), never lowering a counter.
func TestRefreshKeepsIncrementsThatLandDuringIt(t *testing.T) {
	c, _, led, user := newTestCounter(t, time.UTC, nov24h)
	ctx := context.Background()
	b := Budgets{Tokens: TokenCaps{Day: 100}}
	mustCheck(t, c, user, b, false, "build")
	_ = c.rdb.Del(ctx, freshKey(user, Day, nov24)).Err()
	var addErr error
	c.afterSum = func() { addErr = c.Add(ctx, user, nov24h, 100, 7) } // runs in the refresh goroutine
	mustCheck(t, c, user, b, false, "stale; refresh starts")
	c.bg.Wait()
	c.afterSum = nil
	if addErr != nil {
		t.Fatalf("add: %v", addErr)
	}
	mustCheck(t, c, user, b, true, "the increment survived the refresh")

	// And a ledger sum above the counter raises it.
	led.rows = []ledgerRow{{ts: nov24h, tokens: 250, costMicros: 3}}
	_ = c.rdb.Del(ctx, freshKey(user, Day, nov24)).Err()
	mustCheck(t, c, user, b, true, "stale again")
	c.bg.Wait()
	got, _ := c.rdb.HMGet(ctx, dayHash(user), "tok", "cost").Result()
	if fmt.Sprint(got) != "[250 7]" {
		t.Errorf("hash = %v, want tok raised to 250 and cost kept at 7", got)
	}
}

// TestPublisherWithoutTheLockWritesNothing: a rebuild whose lock expired
// and was taken by another replica before it published leaves the counters
// alone, both on the cold path and in a background refresh.
func TestPublisherWithoutTheLockWritesNothing(t *testing.T) {
	c, _, led, user := newTestCounter(t, time.UTC, nov24h)
	ctx := context.Background()
	led.rows = []ledgerRow{{ts: nov24h, tokens: 500}}
	b := Budgets{Tokens: TokenCaps{Day: 100}}
	lock := lockKey(user, Day, nov24)
	c.afterSum = func() { _ = c.rdb.Set(ctx, lock, "another-replica", time.Minute).Err() }

	mustCheck(t, c, user, b, true, "cold path: its own sum is still enforced")
	if n, _ := c.rdb.Exists(ctx, dayHash(user)).Result(); n != 0 {
		t.Error("published without the lock")
	}
	if v, _ := c.rdb.Get(ctx, lock).Result(); v != "another-replica" {
		t.Errorf("lock = %q, want the other replica's left alone", v)
	}

	_ = c.rdb.Del(ctx, lock).Err()
	_ = c.rdb.HSet(ctx, dayHash(user), "tok", 10, "cost", 0).Err()
	mustCheck(t, c, user, b, false, "stale hash, background refresh")
	c.bg.Wait()
	if v, _ := c.rdb.HGet(ctx, dayHash(user), "tok").Int64(); v != 10 {
		t.Errorf("tok = %d, want 10 left alone by a publisher without the lock", v)
	}
}

// TestCorruptCounterIsRebuilt: a hash with a non-numeric field counts as
// missing and is rebuilt from the ledger, replacing the bad value.
func TestCorruptCounterIsRebuilt(t *testing.T) {
	c, _, led, user := newTestCounter(t, time.UTC, nov24h)
	ctx := context.Background()
	led.rows = []ledgerRow{{ts: nov24h, tokens: 500}}
	_ = c.rdb.HSet(ctx, dayHash(user), "tok", "garbage", "cost", "0").Err()
	_ = c.rdb.Set(ctx, freshKey(user, Day, nov24), 1, time.Minute).Err()
	if err := c.Add(ctx, user, nov24h, 1, 1); err != nil {
		t.Errorf("add on a corrupt hash: %v", err)
	}
	mustCheck(t, c, user, Budgets{Tokens: TokenCaps{Day: 100}}, true, "rebuilt from the ledger")
	if v, _ := c.rdb.HGet(ctx, dayHash(user), "tok").Result(); v != "500" {
		t.Errorf("tok = %q, want the ledger's 500", v)
	}
	// A hash missing a field (e.g. a hand edit) is rebuilt the same way.
	_ = c.rdb.HDel(ctx, dayHash(user), "cost").Err()
	if u, err := c.Usage(ctx, user, nov24h); err != nil || u[Day].Tokens != 500 {
		t.Errorf("usage = %+v (%v)", u, err)
	}
	// All digits but beyond int64: Go can't parse it, and the publish must
	// replace it rather than keep it as "higher" forever.
	_ = c.rdb.HSet(ctx, dayHash(user), "tok", "99999999999999999999999999", "cost", "0").Err()
	_ = c.rdb.Del(ctx, freshKey(user, Day, nov24)).Err()
	if u, err := c.Usage(ctx, user, nov24h); err != nil || u[Day].Tokens != 500 {
		t.Errorf("usage after an overflowing field = %+v (%v)", u, err)
	}
	if v, _ := c.rdb.HGet(ctx, dayHash(user), "tok").Result(); v != "500" {
		t.Errorf("overflowing tok = %q, want it replaced by the ledger's 500", v)
	}
}

// TestColdPathLoserUsesThePublishedCounters: without counters, a check that
// does not get the lock waits for the replica building them and enforces
// what it publishes, reading no ledger itself.
func TestColdPathLoserUsesThePublishedCounters(t *testing.T) {
	c, _, led, user := newTestCounter(t, time.UTC, nov24h)
	ctx := context.Background()
	shorten(t, &coldWait, 10*time.Second)
	if err := c.rdb.Set(ctx, lockKey(user, Day, nov24), "another-replica", time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = c.rdb.HSet(ctx, dayHash(user), "tok", 500, "cost", 0).Err()
	}()
	mustCheck(t, c, user, Budgets{Tokens: TokenCaps{Day: 100}}, true, "the other replica's counters")
	if led.asks() != 0 {
		t.Error("the loser read the ledger")
	}
}

// TestColdPathLoserReadsTheLedgerWhenTheWinnerIsSlow: if nothing is
// published within coldWait, the loser reads the ledger itself, enforces
// that, and writes nothing; /api/me's read does the same.
func TestColdPathLoserReadsTheLedgerWhenTheWinnerIsSlow(t *testing.T) {
	c, _, led, user := newTestCounter(t, time.UTC, nov24h)
	ctx := context.Background()
	shorten(t, &coldWait, 100*time.Millisecond)
	led.rows = []ledgerRow{{ts: nov24h, tokens: 500}}
	for _, p := range Periods {
		start, _ := Bounds(p, nov24h, time.UTC)
		_ = c.rdb.Set(ctx, lockKey(user, p, start), "another-replica", time.Minute).Err()
	}
	mustCheck(t, c, user, Budgets{Tokens: TokenCaps{Day: 100}}, true, "direct ledger read")
	if n, _ := c.rdb.Exists(ctx, dayHash(user)).Result(); n != 0 {
		t.Error("the loser wrote counters")
	}
	if u, err := c.Usage(ctx, user, nov24h); err != nil || u[Day].Tokens != 500 || u[Month].Tokens != 500 {
		t.Errorf("usage = %+v (%v), want 500 from the ledger", u, err)
	}
	led.err = errors.New("db down")
	if _, denied, err := c.Check(ctx, user, Budgets{Tokens: TokenCaps{Day: 100}}); denied || err == nil {
		t.Errorf("ledger down too: denied=%v err=%v, want allowed with the error", denied, err)
	}
}

// TestRebuildIsSingleFlight: a burst of concurrent checks with no counters
// costs one ledger read, and every one of them is enforced.
func TestRebuildIsSingleFlight(t *testing.T) {
	c, _, led, user := newTestCounter(t, time.UTC, nov24h)
	ctx := context.Background()
	shorten(t, &coldWait, 10*time.Second)
	led.rows = []ledgerRow{{ts: nov24h, tokens: 500}}
	led.gate = make(chan struct{})
	var wg sync.WaitGroup
	denied := make(chan bool, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, d, _ := c.Check(ctx, user, Budgets{Tokens: TokenCaps{Day: 100}})
			denied <- d
		}()
	}
	close(led.gate)
	wg.Wait()
	close(denied)
	if n := led.asks(); n != 1 {
		t.Errorf("a burst of 20 checks read the ledger %d times, want 1", n)
	}
	for d := range denied {
		if !d {
			t.Error("a check in the burst let an over-cap request through")
		}
	}
	if n, _ := c.rdb.Exists(ctx, lockKey(user, Day, nov24)).Result(); n != 0 {
		t.Error("rebuild lock left behind")
	}
}

// TestRebuildUsesWhatAnotherReplicaJustWrote: a rebuild that gets the lock
// after another replica published returns the stored counters without
// reading the ledger.
func TestRebuildUsesWhatAnotherReplicaJustWrote(t *testing.T) {
	c, _, led, user := newTestCounter(t, time.UTC, nov24h)
	ctx := context.Background()
	_ = c.rdb.HSet(ctx, dayHash(user), "tok", 42, "cost", 7).Err()
	_ = c.rdb.Set(ctx, freshKey(user, Day, nov24), 1, time.Minute).Err()
	u, err := c.rebuild(ctx, user, Day, nov24h)
	if err != nil || u != (Usage{42, 7}) || led.asks() != 0 {
		t.Errorf("rebuild = %+v, %v after %d ledger reads, want the stored 42/7 and none", u, err, led.asks())
	}
}

// TestRebuildBoundsTheLedgerRead: a ledger read that hangs is given up
// after ledgerTimeout, the request is let through, and the lock released.
func TestRebuildBoundsTheLedgerRead(t *testing.T) {
	c, _, led, user := newTestCounter(t, time.UTC, nov24h)
	shorten(t, &ledgerTimeout, 100*time.Millisecond)
	led.block = true
	_, denied, err := c.Check(context.Background(), user, Budgets{Tokens: TokenCaps{Day: 1}})
	if denied || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("denied=%v err=%v, want allowed with a deadline error", denied, err)
	}
	if n, _ := c.rdb.Exists(context.Background(), lockKey(user, Day, nov24)).Result(); n != 0 {
		t.Error("lock left behind")
	}
}

// TestFailedRefreshBacksOff: a background refresh that cannot read the
// ledger keeps the counters enforced and holds off the next attempt.
func TestFailedRefreshBacksOff(t *testing.T) {
	c, _, led, user := newTestCounter(t, time.UTC, nov24h)
	ctx := context.Background()
	led.rows = []ledgerRow{{ts: nov24h, tokens: 500}}
	b := Budgets{Tokens: TokenCaps{Day: 100}}
	mustCheck(t, c, user, b, true, "build")
	_ = c.rdb.Del(ctx, freshKey(user, Day, nov24)).Err()
	led.err = errors.New("db down")
	mustCheck(t, c, user, b, true, "stale, still enforced")
	c.bg.Wait()
	ttl, err := c.rdb.PTTL(ctx, freshKey(user, Day, nov24)).Result()
	if err != nil || ttl <= 0 || ttl > retryAfter {
		t.Errorf("backoff marker TTL = %v (%v), want at most %v", ttl, err, retryAfter)
	}
	asked := led.asks()
	mustCheck(t, c, user, b, true, "during the backoff")
	c.bg.Wait()
	if led.asks() != asked {
		t.Error("refresh retried during the backoff")
	}
}

// TestUsageReadsTheCounters: the dashboard's numbers come from the counters
// (built once), and include usage settled since.
func TestUsageReadsTheCounters(t *testing.T) {
	c, _, led, user := newTestCounter(t, time.UTC, nov24h)
	led.rows = []ledgerRow{{ts: nov24h, tokens: 10, costMicros: 1000}, {ts: time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC), tokens: 5}}
	u, err := c.Usage(context.Background(), user, nov24h)
	if err != nil {
		t.Fatal(err)
	}
	if u[Day] != (Usage{10, 1000}) || u[Month] != (Usage{15, 1000}) {
		t.Fatalf("usage %+v", u)
	}
	mustAdd(t, c, user, 1, 1)
	asked := led.asks()
	u, _ = c.Usage(context.Background(), user, nov24h)
	if u[Day] != (Usage{11, 1001}) || led.asks() != asked {
		t.Errorf("usage %+v after %d more ledger reads, want 11/1001 from the counters alone", u, led.asks()-asked)
	}
}

// TestAddCountsInThePeriodOfItsTimestamp: a request stamped just before
// midnight and settled just after counts toward the day it was stamped in,
// as its ledger row does.
func TestAddCountsInThePeriodOfItsTimestamp(t *testing.T) {
	day1 := time.Date(2026, 11, 24, 23, 59, 59, 0, time.UTC)
	c, clk, _, user := newTestCounter(t, time.UTC, day1)
	b := Budgets{Tokens: TokenCaps{Day: 100}}
	mustCheck(t, c, user, b, false, "day 1")
	clk.set(day1.Add(2 * time.Second))
	mustCheck(t, c, user, b, false, "day 2")
	if err := c.Add(context.Background(), user, day1, 100, 0); err != nil {
		t.Fatal(err)
	}
	mustCheck(t, c, user, b, false, "day 2 is untouched")
	clk.set(day1)
	mustCheck(t, c, user, b, true, "day 1 got the usage")
}
