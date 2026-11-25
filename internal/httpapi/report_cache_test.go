package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"
)

func TestReportCache(t *testing.T) {
	var c reportCache
	t0 := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	loads := 0
	load := func() (any, error) { loads++; return loads, nil }
	get := func(now time.Time) any {
		v, err := c.get("k", now, load)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}

	if v := get(t0); v != 1 {
		t.Fatalf("first get = %v", v)
	}
	if v := get(t0.Add(reportCacheTTL - time.Second)); v != 1 || loads != 1 {
		t.Errorf("within the TTL: %v after %d loads, want the cached 1", v, loads)
	}
	if v := get(t0.Add(reportCacheTTL)); v != 2 {
		t.Errorf("at the TTL: %v, want a reload", v)
	}
	c.purge()
	if v := get(t0.Add(reportCacheTTL)); v != 3 {
		t.Errorf("after purge: %v, want a reload", v)
	}

	// Errors are not cached.
	if _, err := c.get("e", t0, func() (any, error) { return nil, errors.New("db down") }); err == nil {
		t.Fatal("error swallowed")
	}
	if v, err := c.get("e", t0, func() (any, error) { return "ok", nil }); err != nil || v != "ok" {
		t.Errorf("after an error: %v %v, want a fresh load", v, err)
	}

	// A load that a purge overtakes is answered but not kept.
	v, _ := c.get("p", t0, func() (any, error) { c.purge(); return "stale", nil })
	if v != "stale" {
		t.Fatalf("got %v", v)
	}
	if v, _ := c.get("p", t0, func() (any, error) { return "fresh", nil }); v != "fresh" {
		t.Errorf("a load overtaken by a purge was cached: %v", v)
	}
}

// TestGroupReportAndSpendAreCached: polling the group report and the users
// list within the TTL reuses the aggregate; it is refreshed after the TTL,
// and at once after a control-plane write.
func TestGroupReportAndSpendAreCached(t *testing.T) {
	f := newBudgetFixture(t)
	s := f.server(t)
	asAdmin(s)
	uid := f.user(t, "cached")
	at := time.Date(2026, 10, 3, 1, 0, 0, 0, time.UTC)
	f.ledgerRow(t, uid, at, 10, "1")

	total := func() int64 {
		t.Helper()
		rec := call(t, s, http.MethodGet, "/api/admin/usage/groups?period=day", "")
		wantStatus(t, rec, http.StatusOK, "report")
		var rep struct {
			Total usageTotals `json:"total"`
		}
		decodeBody(t, rec, &rep)
		return rep.Total.Tokens
	}
	daySpend := func() int64 { return listedUser(t, s, uid).Spend["day"].Tokens }

	before, spendBefore := total(), daySpend()
	if spendBefore != 10 {
		t.Fatalf("day spend = %d, want 10", spendBefore)
	}
	f.ledgerRow(t, uid, at.Add(time.Minute), 5, "0")
	if got := total(); got != before {
		t.Errorf("report within the TTL = %d, want the cached %d", got, before)
	}
	if got := daySpend(); got != 10 {
		t.Errorf("users list within the TTL = %d, want the cached 10", got)
	}

	f.clock.Advance(reportCacheTTL)
	if got := total(); got != before+5 {
		t.Errorf("report after the TTL = %d, want %d", got, before+5)
	}
	if got := daySpend(); got != 15 {
		t.Errorf("users list after the TTL = %d, want 15", got)
	}

	// A write purges at once: a new group shows in the next report.
	f.ledgerRow(t, uid, at.Add(2*time.Minute), 7, "0")
	rec := call(t, s, http.MethodPost, "/api/admin/groups", fmt.Sprintf(`{"name":"cache-%s"}`, f.suffix))
	wantStatus(t, rec, http.StatusCreated, "create group")
	var g groupView
	decodeBody(t, rec, &g)
	t.Cleanup(func() { _, _ = f.pool.Exec(context.Background(), `DELETE FROM user_groups WHERE id = $1`, g.ID) })
	if got := total(); got != before+12 {
		t.Errorf("report after a write = %d, want %d", got, before+12)
	}
	if got := daySpend(); got != 22 {
		t.Errorf("users list after a write = %d, want 22", got)
	}
}
