package budget

import (
	"strings"
	"testing"
	"time"
)

func mustLoad(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("load %s: %v", name, err)
	}
	return loc
}

func TestBoundsAndIDs(t *testing.T) {
	berlin := mustLoad(t, "Europe/Berlin")
	newYork := mustLoad(t, "America/New_York")
	cases := []struct {
		name       string
		p          Period
		at         time.Time
		start, end time.Time
		id         string
	}{
		{"utc day", Day, time.Date(2026, 11, 24, 15, 0, 0, 0, time.UTC),
			time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC), "2026-11-24"},
		{"utc week starts monday", Week, time.Date(2026, 11, 24, 15, 0, 0, 0, time.UTC),
			time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 30, 0, 0, 0, 0, time.UTC), "2026-W48"},
		{"sunday belongs to the week before", Week, time.Date(2026, 11, 29, 23, 59, 0, 0, time.UTC),
			time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 30, 0, 0, 0, 0, time.UTC), "2026-W48"},
		{"utc month", Month, time.Date(2026, 11, 24, 15, 0, 0, 0, time.UTC),
			time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC), "2026-11"},
		{"december rolls into the next year", Month, time.Date(2026, 12, 31, 23, 0, 0, 0, time.UTC),
			time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC), time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), "2026-12"},
		// 1 Jan 2027 is a Friday: its ISO week is 2026's 53rd.
		{"iso week of new year's day belongs to the old year", Week, time.Date(2027, 1, 1, 12, 0, 0, 0, time.UTC),
			time.Date(2026, 12, 28, 0, 0, 0, 0, time.UTC), time.Date(2027, 1, 4, 0, 0, 0, 0, time.UTC), "2026-W53"},
		// 31 Dec 2025 is a Wednesday: its ISO week is 2026's first.
		{"iso week of new year's eve belongs to the new year", Week, time.Date(2025, 12, 31, 12, 0, 0, 0, time.UTC),
			time.Date(2025, 12, 29, 0, 0, 0, 0, time.UTC), time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC), "2026-W01"},
		// 23:30 UTC on the 24th is already the 25th in Berlin.
		{"day follows the zone, not utc", Day, time.Date(2026, 11, 24, 23, 30, 0, 0, time.UTC),
			time.Date(2026, 11, 25, 0, 0, 0, 0, berlin), time.Date(2026, 11, 26, 0, 0, 0, 0, berlin), "2026-11-25"},
		// Clocks go forward on 29 Mar 2026 in Berlin: a 23-hour day.
		{"spring-forward day", Day, time.Date(2026, 3, 29, 12, 0, 0, 0, berlin),
			time.Date(2026, 3, 29, 0, 0, 0, 0, berlin), time.Date(2026, 3, 30, 0, 0, 0, 0, berlin), "2026-03-29"},
		// Clocks go back on 1 Nov 2026 in New York: a 25-hour day.
		{"fall-back day", Day, time.Date(2026, 11, 1, 23, 30, 0, 0, newYork),
			time.Date(2026, 11, 1, 0, 0, 0, 0, newYork), time.Date(2026, 11, 2, 0, 0, 0, 0, newYork), "2026-11-01"},
		{"week spanning a dst change", Week, time.Date(2026, 11, 1, 12, 0, 0, 0, newYork),
			time.Date(2026, 10, 26, 0, 0, 0, 0, newYork), time.Date(2026, 11, 2, 0, 0, 0, 0, newYork), "2026-W44"},
	}
	for _, c := range cases {
		loc := c.start.Location()
		start, end := Bounds(c.p, c.at, loc)
		if !start.Equal(c.start) || !end.Equal(c.end) {
			t.Errorf("%s: bounds [%v, %v), want [%v, %v)", c.name, start, end, c.start, c.end)
		}
		if id := ID(c.p, start); id != c.id {
			t.Errorf("%s: id %q, want %q", c.name, id, c.id)
		}
	}
	if s, e := Bounds(Day, time.Date(2026, 3, 29, 12, 0, 0, 0, berlin), berlin); e.Sub(s) != 23*time.Hour {
		t.Errorf("spring-forward day lasts %v, want 23h", e.Sub(s))
	}
	if s, e := Bounds(Day, time.Date(2026, 11, 1, 12, 0, 0, 0, newYork), newYork); e.Sub(s) != 25*time.Hour {
		t.Errorf("fall-back day lasts %v, want 25h", e.Sub(s))
	}
}

func TestValidate(t *testing.T) {
	b, err := Validate([]byte(`{"tokens":{"day":0,"week":2000000},"cost_usd":{"day":5,"month":50.5}}`))
	if err != nil {
		t.Fatalf("valid budgets rejected: %v", err)
	}
	want := Budgets{Tokens: TokenCaps{Week: 2000000}, CostUSD: CostCaps{Day: 5, Month: 50.5}}
	if b != want {
		t.Errorf("parsed %+v, want %+v", b, want)
	}
	if b.CostCapMicro(Month) != 50_500_000 {
		t.Errorf("month cost cap = %d micro-USD, want 50500000", b.CostCapMicro(Month))
	}
	for _, ok := range []string{`{}`, `null`, `{"tokens":null}`, `{"cost_usd":{"day":null}}`} {
		if b, err := Validate([]byte(ok)); err != nil || b.Any() {
			t.Errorf("%s: (%+v, %v), want no caps and no error", ok, b, err)
		}
	}
	for _, bad := range []string{
		`{"tokens":{"day":-1}}`,
		`{"cost_usd":{"month":-0.01}}`,
		`{"tokens":{"day":1.5}}`,
		`{"tokens":{"hour":1}}`,
		`{"requests":{"day":1}}`,
		`{"tokens":{"day":"10"}}`,
		`{"cost_usd":{"day":1e300}}`,
		`[]`,
		`{"tokens":[1]}`,
	} {
		if _, err := Validate([]byte(bad)); err == nil {
			t.Errorf("%s: accepted, want an error", bad)
		}
	}
}

func TestParseIsLenient(t *testing.T) {
	if b := Parse([]byte(`{}`)); b.Any() {
		t.Errorf("empty budgets have caps: %+v", b)
	}
	if b := Parse([]byte(`not json`)); b.Any() {
		t.Errorf("unreadable budgets have caps: %+v", b)
	}
	if b := Parse([]byte(`{"tokens":{"month":10}}`)); b.TokenCap(Month) != 10 {
		t.Errorf("month token cap = %d, want 10", b.TokenCap(Month))
	}
}

func TestDenialMessage(t *testing.T) {
	d := Denial{Period: Month, Cost: true, Limit: 50_000_000, ResetsAt: time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)}
	if got, want := d.Message(), "monthly cost budget $50.00 reached; resets 2026-12-01 00:00 UTC"; got != want {
		t.Errorf("message %q, want %q", got, want)
	}
	berlin := mustLoad(t, "Europe/Berlin")
	d = Denial{Period: Day, Limit: 30000, ResetsAt: time.Date(2026, 11, 25, 0, 0, 0, 0, berlin)}
	if got := d.Message(); !strings.HasPrefix(got, "daily token budget 30,000 reached; resets 2026-11-25 00:00 Europe/Berlin") {
		t.Errorf("message %q", got)
	}
	d = Denial{Period: Week, Cost: true, Limit: 1_234_567_890, ResetsAt: time.Date(2026, 11, 30, 0, 0, 0, 0, time.UTC)}
	if got := d.Message(); !strings.HasPrefix(got, "weekly cost budget $1,234.57 reached") {
		t.Errorf("message %q", got)
	}
}

func TestGroupDigits(t *testing.T) {
	for n, want := range map[int64]string{0: "0", 999: "999", 1000: "1,000", 30000: "30,000", 2000000: "2,000,000", -1234: "-1,234"} {
		if got := groupDigits(n); got != want {
			t.Errorf("groupDigits(%d) = %q, want %q", n, got, want)
		}
	}
}
