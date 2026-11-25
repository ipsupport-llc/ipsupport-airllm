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
// was asked for.
type fakeLedger struct {
	mu    sync.Mutex
	rows  []ledgerRow
	asked [][2]time.Time
	err   error
	delay time.Duration // how long each sum takes
	block bool          // each sum waits for its context to end
}

type ledgerRow struct {
	ts         time.Time
	tokens     int64
	costMicros int64
}

func (f *fakeLedger) sum(ctx context.Context, _ string, start, end time.Time) (int64, int64, error) {
	time.Sleep(f.delay)
	if f.block {
		<-ctx.Done()
		return 0, 0, ctx.Err()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, [2]time.Time{start, end})
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

func newTestCounter(t *testing.T, loc *time.Location, at time.Time) (*Counter, *clock, *fakeLedger, string) {
	rdb := testRedis(t)
	clk := &clock{t: at}
	led := &fakeLedger{}
	user := fmt.Sprintf("budget-test-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		ctx := context.Background()
		iter := rdb.Scan(ctx, 0, "air:ub:"+user+":*", 100).Iterator()
		for iter.Next(ctx) {
			_ = rdb.Del(ctx, iter.Val()).Err()
		}
	})
	return NewCounter(rdb, loc, clk.now, led.sum), clk, led, user
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

func (f *fakeLedger) asks() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.asked)
}

var nov24 = time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC)

// TestStaleCountersAreRefreshedFromTheLedger: counters live until their
// period ends; their freshness marker expires after FreshFor, and the next
// check then re-derives them from the ledger, overwriting what drifted —
// here a row the ledger got without a settle.
func TestStaleCountersAreRefreshedFromTheLedger(t *testing.T) {
	at := time.Date(2026, 11, 24, 15, 0, 0, 0, time.UTC)
	c, _, led, user := newTestCounter(t, time.UTC, at)
	ctx := context.Background()
	b := Budgets{Tokens: TokenCaps{Day: 100}}
	mustCheck(t, c, user, b, false, "empty ledger")
	dayKey, fresh := counterKey(user, unitTokens, Day, nov24), freshKey(user, Day, nov24)
	if ttl, err := c.rdb.PTTL(ctx, dayKey).Result(); err != nil || ttl < 9*time.Hour {
		t.Errorf("day counter TTL = %v (%v), want until the day ends + grace", ttl, err)
	}
	if ttl, err := c.rdb.PTTL(ctx, fresh).Result(); err != nil || ttl <= 0 || ttl > FreshFor {
		t.Errorf("fresh marker TTL = %v (%v), want at most %v", ttl, err, FreshFor)
	}

	led.rows = append(led.rows, ledgerRow{ts: at, tokens: 100}) // never settled
	mustCheck(t, c, user, b, false, "drifted counter, still fresh")
	asked := led.asks()
	_ = c.rdb.Del(ctx, fresh).Err() // what its TTL does
	mustCheck(t, c, user, b, true, "refreshed from the ledger")
	if led.asks() != asked+1 {
		t.Errorf("refresh read the ledger %d times, want once", led.asks()-asked)
	}
	if v, _ := c.rdb.Get(ctx, dayKey).Int64(); v != 100 {
		t.Errorf("day counter = %d after the refresh, want the ledger's 100", v)
	}
}

// TestStaleCountersStayEnforcedDuringARefresh: an over-cap user whose
// counters are due for a refresh another replica is doing right now is still
// refused — the refresh never opens a window without a check.
func TestStaleCountersStayEnforcedDuringARefresh(t *testing.T) {
	at := time.Date(2026, 11, 24, 15, 0, 0, 0, time.UTC)
	c, _, led, user := newTestCounter(t, time.UTC, at)
	ctx := context.Background()
	led.rows = []ledgerRow{{ts: at, tokens: 500}}
	b := Budgets{Tokens: TokenCaps{Day: 100}}
	mustCheck(t, c, user, b, true, "over the cap")
	_ = c.rdb.Del(ctx, freshKey(user, Day, nov24)).Err()
	if err := c.rdb.Set(ctx, lockKey(user, Day, nov24), "someone-else", time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	asked := led.asks()
	start := time.Now()
	mustCheck(t, c, user, b, true, "stale counters, refresh running elsewhere")
	if led.asks() != asked {
		t.Error("read the ledger without holding the lock")
	}
	if time.Since(start) > waitMax/2 {
		t.Errorf("check waited %v although counters exist", time.Since(start))
	}
}

// TestMissingCountersWaitForTheRebuildingReplica: with no counters at all,
// a check that does not get the lock waits for the replica rebuilding them
// and enforces what it wrote; if nothing appears in time it lets the
// request through.
func TestMissingCountersWaitForTheRebuildingReplica(t *testing.T) {
	at := time.Date(2026, 11, 24, 15, 0, 0, 0, time.UTC)
	c, _, led, user := newTestCounter(t, time.UTC, at)
	ctx := context.Background()
	led.rows = []ledgerRow{{ts: at, tokens: 500}}
	b := Budgets{Tokens: TokenCaps{Day: 100}}
	lock := lockKey(user, Day, nov24)
	if lock != "air:ub:"+user+":lock:2026-11-24" {
		t.Errorf("lock key %q", lock)
	}
	if err := c.rdb.Set(ctx, lock, "someone-else", time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(250 * time.Millisecond)
		_ = c.rdb.Set(ctx, counterKey(user, unitTokens, Day, nov24), 500, time.Hour).Err()
		_ = c.rdb.Set(ctx, counterKey(user, unitCost, Day, nov24), 0, time.Hour).Err()
	}()
	mustCheck(t, c, user, b, true, "the other replica's counters appear while waiting")
	if led.asks() != 0 {
		t.Error("read the ledger without holding the lock")
	}
	// Dashboard reads use the same wait instead of reading the ledger.
	if u, err := c.Usage(ctx, user, at); err != nil || u[Day].Tokens != 500 {
		t.Errorf("usage = %+v (%v), want the counters' 500", u, err)
	}

	user2 := user + "-2"
	if err := c.rdb.Set(ctx, lockKey(user2, Day, nov24), "someone-else", time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	mustCheck(t, c, user2, b, false, "nothing appears: fail open")
	if el := time.Since(start); el < waitMax-waitStep || el > waitMax+time.Second {
		t.Errorf("waited %v, want about %v", el, waitMax)
	}
	if _, err := c.Usage(ctx, user2, at); err == nil {
		t.Error("usage without counters answered without an error")
	}
}

// TestRebuildIsSingleFlight: a burst of concurrent checks with no counters
// costs one ledger read, and every one of them is enforced.
func TestRebuildIsSingleFlight(t *testing.T) {
	at := time.Date(2026, 11, 24, 15, 0, 0, 0, time.UTC)
	c, _, led, user := newTestCounter(t, time.UTC, at)
	ctx := context.Background()
	led.rows = []ledgerRow{{ts: at, tokens: 500}}
	led.delay = 100 * time.Millisecond
	b := Budgets{Tokens: TokenCaps{Day: 100}}
	var wg sync.WaitGroup
	denied := make(chan bool, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, d, _ := c.Check(ctx, user, b)
			denied <- d
		}()
	}
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
	if exists, _ := c.rdb.Exists(ctx, lockKey(user, Day, nov24)).Result(); exists != 0 {
		t.Error("rebuild lock left behind")
	}
}

// TestRebuildUsesWhatAnotherReplicaJustWrote: a rebuild that gets the lock
// after another replica finished one returns the stored counters without
// reading the ledger.
func TestRebuildUsesWhatAnotherReplicaJustWrote(t *testing.T) {
	at := time.Date(2026, 11, 24, 15, 0, 0, 0, time.UTC)
	c, _, led, user := newTestCounter(t, time.UTC, at)
	ctx := context.Background()
	led.rows = []ledgerRow{{ts: at, tokens: 999}}
	_ = c.rdb.Set(ctx, counterKey(user, unitTokens, Day, nov24), 42, time.Hour).Err()
	_ = c.rdb.Set(ctx, counterKey(user, unitCost, Day, nov24), 7, time.Hour).Err()
	_ = c.rdb.Set(ctx, freshKey(user, Day, nov24), 1, time.Minute).Err()
	u, err := c.rebuild(ctx, user, Day, at)
	if err != nil || u != (Usage{42, 7}) || led.asks() != 0 {
		t.Errorf("rebuild = %+v, %v after %d ledger reads, want the stored 42/7 and none", u, err, led.asks())
	}
}

// TestRebuildBoundsTheLedgerRead: a ledger read that hangs is given up
// after ledgerTimeout, the request is let through, and the lock released.
func TestRebuildBoundsTheLedgerRead(t *testing.T) {
	at := time.Date(2026, 11, 24, 15, 0, 0, 0, time.UTC)
	c, _, led, user := newTestCounter(t, time.UTC, at)
	defer func(d time.Duration) { ledgerTimeout = d }(ledgerTimeout)
	ledgerTimeout = 100 * time.Millisecond
	led.block = true
	start := time.Now()
	_, denied, err := c.Check(context.Background(), user, Budgets{Tokens: TokenCaps{Day: 1}})
	if denied || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("denied=%v err=%v, want allowed with a deadline error", denied, err)
	}
	if el := time.Since(start); el > time.Second {
		t.Errorf("check took %v", el)
	}
	if n, _ := c.rdb.Exists(context.Background(), lockKey(user, Day, nov24)).Result(); n != 0 {
		t.Error("lock left behind")
	}
}

// TestUsageReadsTheCounters: the dashboard's numbers come from the counters
// (rebuilt once), and include usage settled since.
func TestUsageReadsTheCounters(t *testing.T) {
	at := time.Date(2026, 11, 24, 15, 0, 0, 0, time.UTC)
	c, _, led, user := newTestCounter(t, time.UTC, at)
	led.rows = []ledgerRow{{ts: at, tokens: 10, costMicros: 1000}, {ts: time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC), tokens: 5}}
	u, err := c.Usage(context.Background(), user, at)
	if err != nil {
		t.Fatal(err)
	}
	if u[Day] != (Usage{10, 1000}) || u[Month] != (Usage{15, 1000}) {
		t.Fatalf("usage %+v", u)
	}
	mustAdd(t, c, user, 1, 1)
	asked := led.asks()
	u, _ = c.Usage(context.Background(), user, at)
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
