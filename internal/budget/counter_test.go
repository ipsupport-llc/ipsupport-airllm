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
}

type ledgerRow struct {
	ts         time.Time
	tokens     int64
	costMicros int64
}

func (f *fakeLedger) sum(_ context.Context, _ string, start, end time.Time) (int64, int64, error) {
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
	if err := c.Add(context.Background(), user, tokens, cost); err != nil {
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
