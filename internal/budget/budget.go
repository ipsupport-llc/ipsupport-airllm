// Package budget enforces per-user spending caps over calendar periods (day,
// ISO week, month in a configured time zone), summed across all of a user's
// API keys.
//
// Usage is counted in Redis, one counter per user, unit and period. The
// counters are a cache of the usage ledger: a counter that is missing when a
// request is checked is rebuilt from the ledger and created with SET NX, and
// settling a request only increments counters that already exist, so a
// counter is never created holding just the latest request. Enforcement is
// check-before only, with no reservation: cost is known only once a response
// has finished, so one request may overshoot a cap and the next one is
// refused. Redis or ledger trouble fails open.
package budget

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"
)

// Period is a calendar budget period.
type Period int

// The budget periods, shortest first.
const (
	Day Period = iota
	Week
	Month
)

// Periods lists every period, shortest first.
var Periods = []Period{Day, Week, Month}

// Name is the period's JSON key: "day", "week" or "month".
func (p Period) Name() string {
	switch p {
	case Day:
		return "day"
	case Week:
		return "week"
	default:
		return "month"
	}
}

// adjective names the period in a client-facing message.
func (p Period) adjective() string {
	switch p {
	case Day:
		return "daily"
	case Week:
		return "weekly"
	default:
		return "monthly"
	}
}

// code is the period's segment in a counter key.
func (p Period) code() string {
	switch p {
	case Day:
		return "d"
	case Week:
		return "w"
	default:
		return "m"
	}
}

// Bounds returns the period containing t in loc: [start, end). Day starts at
// 00:00, week on Monday 00:00 (ISO week), month on the 1st at 00:00. The
// bounds are computed on the calendar, not by adding fixed durations, so a
// day that a DST change makes 23 or 25 hours long is still one day.
func Bounds(p Period, t time.Time, loc *time.Location) (start, end time.Time) {
	t = t.In(loc)
	y, m, d := t.Date()
	switch p {
	case Day:
		return time.Date(y, m, d, 0, 0, 0, 0, loc), time.Date(y, m, d+1, 0, 0, 0, 0, loc)
	case Week:
		back := (int(t.Weekday()) + 6) % 7 // days since Monday
		return time.Date(y, m, d-back, 0, 0, 0, 0, loc), time.Date(y, m, d-back+7, 0, 0, 0, 0, loc)
	default:
		return time.Date(y, m, 1, 0, 0, 0, 0, loc), time.Date(y, m+1, 1, 0, 0, 0, 0, loc)
	}
}

// ID names the period that starts at start (as returned by Bounds):
// 2026-11-24, 2026-W48 or 2026-11.
func ID(p Period, start time.Time) string {
	switch p {
	case Day:
		return start.Format("2006-01-02")
	case Week:
		// The Monday's ISO year, not its calendar year: the week holding
		// 1 January can belong to the year before or after.
		y, w := start.ISOWeek()
		return fmt.Sprintf("%04d-W%02d", y, w)
	default:
		return start.Format("2006-01")
	}
}

// TokenCaps are token caps per period; 0 = no cap.
type TokenCaps struct {
	Day   int64 `json:"day"`
	Week  int64 `json:"week"`
	Month int64 `json:"month"`
}

// CostCaps are cost caps in USD per period; 0 = no cap.
type CostCaps struct {
	Day   float64 `json:"day"`
	Week  float64 `json:"week"`
	Month float64 `json:"month"`
}

// Budgets are one user's caps, as stored in users.budgets.
type Budgets struct {
	Tokens  TokenCaps `json:"tokens"`
	CostUSD CostCaps  `json:"cost_usd"`
}

// TokenCap returns the token cap for p (0 = none).
func (b Budgets) TokenCap(p Period) int64 {
	switch p {
	case Day:
		return b.Tokens.Day
	case Week:
		return b.Tokens.Week
	default:
		return b.Tokens.Month
	}
}

// CostCap returns the cost cap for p in USD (0 = none).
func (b Budgets) CostCap(p Period) float64 {
	switch p {
	case Day:
		return b.CostUSD.Day
	case Week:
		return b.CostUSD.Week
	default:
		return b.CostUSD.Month
	}
}

// CostCapMicro returns the cost cap for p in micro-USD (0 = none).
func (b Budgets) CostCapMicro(p Period) int64 {
	return int64(math.Round(b.CostCap(p) * 1e6))
}

// Any reports whether at least one cap is set.
func (b Budgets) Any() bool {
	for _, p := range Periods {
		if b.TokenCap(p) > 0 || b.CostCapMicro(p) > 0 {
			return true
		}
	}
	return false
}

// Parse reads a stored budgets value. A value that cannot be read means no
// caps: it was written through Validate, so this only guards against a hand
// edit, and failing open matches the rest of the enforcement.
func Parse(raw []byte) Budgets {
	var b Budgets
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &b)
	}
	return b
}

// maxCostUSD bounds a cost cap so its micro-USD value fits an int64 with
// plenty of room.
const maxCostUSD = 1e12

// Validate strictly parses a budgets object from an admin request: only the
// keys "tokens" and "cost_usd", each holding only "day", "week" and "month";
// every value a non-negative number, and a whole number for tokens. A
// missing key or null means no cap.
func Validate(raw []byte) (Budgets, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return Budgets{}, errors.New("budgets must be an object")
	}
	var out Budgets
	for unit, body := range top {
		if unit != "tokens" && unit != "cost_usd" {
			return Budgets{}, fmt.Errorf("budgets: unknown key %q (want tokens, cost_usd)", unit)
		}
		if bytes.Equal(bytes.TrimSpace(body), []byte("null")) {
			continue
		}
		var periods map[string]any
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.UseNumber()
		if err := dec.Decode(&periods); err != nil {
			return Budgets{}, fmt.Errorf("budgets.%s must be an object of numbers", unit)
		}
		for name, v := range periods {
			p, ok := periodByName(name)
			if !ok {
				return Budgets{}, fmt.Errorf("budgets.%s: unknown key %q (want day, week, month)", unit, name)
			}
			if v == nil {
				continue
			}
			num, ok := v.(json.Number)
			if !ok {
				return Budgets{}, fmt.Errorf("budgets.%s.%s must be a number", unit, name)
			}
			if unit == "tokens" {
				n, err := num.Int64()
				if err != nil || n < 0 {
					return Budgets{}, fmt.Errorf("budgets.tokens.%s must be a non-negative whole number", name)
				}
				out.setTokens(p, n)
				continue
			}
			f, err := num.Float64()
			if err != nil || f < 0 || f > maxCostUSD || math.IsNaN(f) {
				return Budgets{}, fmt.Errorf("budgets.cost_usd.%s must be a non-negative number of dollars", name)
			}
			out.setCost(p, f)
		}
	}
	return out, nil
}

func periodByName(name string) (Period, bool) {
	for _, p := range Periods {
		if p.Name() == name {
			return p, true
		}
	}
	return 0, false
}

func (b *Budgets) setTokens(p Period, n int64) {
	switch p {
	case Day:
		b.Tokens.Day = n
	case Week:
		b.Tokens.Week = n
	default:
		b.Tokens.Month = n
	}
}

func (b *Budgets) setCost(p Period, f float64) {
	switch p {
	case Day:
		b.CostUSD.Day = f
	case Week:
		b.CostUSD.Week = f
	default:
		b.CostUSD.Month = f
	}
}
