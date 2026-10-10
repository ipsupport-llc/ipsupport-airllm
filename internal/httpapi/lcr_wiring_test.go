package httpapi

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/lcr"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/pricing"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/providers"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/routing"
)

// TestBuildLCRRankUsesPriceAndAvailability exercises the pure rank-building
// helper in isolation (no Redis, no DB) — it takes a pricing lookup and an
// availability snapshot and produces the routing.LCRRank the ordering code
// in Task 3 consumes.
func TestBuildLCRRankUsesPriceAndAvailability(t *testing.T) {
	priceOf := func(provider, model string) (float64, bool) {
		if provider == "known" {
			return 3.0, true
		}
		return 0, false
	}
	avail := map[lcr.Key]float64{{Provider: "known", UpstreamModel: "m"}: 0.75}
	rank := buildLCRRank(priceOf, avail)

	got := rank(routing.Target{Provider: "known", UpstreamModel: "m"})
	if got.Price != 3.0 || got.Availability != 0.75 {
		t.Errorf("rank(known) = %+v, want {Price:3 Availability:0.75}", got)
	}

	gotUnknown := rank(routing.Target{Provider: "unknown", UpstreamModel: "m"})
	if !math.IsInf(gotUnknown.Price, 1) {
		t.Errorf("rank(unknown).Price = %v, want +Inf", gotUnknown.Price)
	}
	if gotUnknown.Availability != 1.0 {
		t.Errorf("rank(unknown, no avail entry).Availability = %v, want 1.0 (cold start)", gotUnknown.Availability)
	}
}

// TestLCRStrategyServesCheaperTargetFirst is the integration seam Task 4
// left untested: a real lcr-strategy, lcr_mode=price alias, driven through
// the actual executePlan (via runChat, the same harness failover_test.go and
// breaker_test.go use), must actually serve the cheaper of two targets —
// not just rank correctly in orderTierLCR's own isolated unit tests.
func TestLCRStrategyServesCheaperTargetFirst(t *testing.T) {
	cheap := fmt.Sprintf("lcr-cheap-%d", time.Now().UnixNano())
	pricey := fmt.Sprintf("lcr-pricey-%d", time.Now().UnixNano())
	s := newRunChatTestServer(t, providers.NewMock(cheap), providers.NewMock(pricey))
	s.pricing = pricing.New()
	s.pricing.Set(cheap, "m", pricing.Price{InputPer1M: 1, OutputPer1M: 1})
	s.pricing.Set(pricey, "m", pricing.Price{InputPer1M: 50, OutputPer1M: 50})
	// s.lcr stays nil on purpose: lcr_mode=price never reads a Snapshot, and
	// Store's nil-receiver safety (internal/lcr) covers the Record calls
	// executePlan still makes on success.

	plan := &routing.Plan{
		Alias: "lcr-price-test", Strategy: "lcr", LCRMode: "price",
		Tiers: [][]routing.Target{{
			// Array order deliberately puts the pricier target first — only
			// real LCR ranking through the ordering step can put cheap
			// ahead of it.
			{Provider: pricey, UpstreamModel: "m", Tier: 0},
			{Provider: cheap, UpstreamModel: "m", Tier: 0},
		}},
	}
	_, res, err := s.runChat(context.Background(), plan, hiReq)
	if err != nil {
		t.Fatalf("runChat: %v", err)
	}
	if res.Provider != cheap {
		t.Fatalf("served by %q, want the cheaper target %q", res.Provider, cheap)
	}
}

// TestLCRStrategyRecordsFailureDropsAvailability proves the other half of
// the executePlan/lcr seam: a tier-attributable failure on an lcr-strategy
// alias actually reaches s.lcr.Record via the real failure branch in
// executePlan (not just buildLCRRank's own isolated unit test above), and
// the EWMA availability a later Snapshot reports for that target actually
// moves off its 1.0 cold start. Uses the disposable Redis every other real-
// Redis test in this package uses (testBreakerRedis, defined in
// breaker_test.go) since Record's write is fire-and-forget and a nil Redis
// client would make it a no-op, defeating the point of this test.
func TestLCRStrategyRecordsFailureDropsAvailability(t *testing.T) {
	rdb := testBreakerRedis(t)
	flaky := fmt.Sprintf("lcr-flaky-%d", time.Now().UnixNano())
	s := newRunChatTestServer(t, providers.NewMock(flaky))
	s.pricing = pricing.New()
	s.lcr = lcr.New(rdb)
	key := lcr.Key{Provider: flaky, UpstreamModel: "fail-m"}
	t.Cleanup(func() { _ = rdb.Del(context.Background(), "air:lcr:avail:"+flaky+":fail-m").Err() })

	plan := &routing.Plan{
		Alias: "lcr-fail-test", Strategy: "lcr", LCRMode: "price",
		// "fail-m" is providers.Mock's trigger for a retryable 503 — a
		// generic failure that counts against the tier (unlike ctxfail/
		// novision/noreasoning), so it must hit the same
		// countsAgainstTier branch that calls s.lcr.Record(..., false).
		Tiers: [][]routing.Target{{{Provider: flaky, UpstreamModel: "fail-m", Tier: 0}}},
	}
	if _, _, err := s.runChat(context.Background(), plan, hiReq); err == nil {
		t.Fatal("expected the only target's failure to fail the whole request")
	}

	// Record's write is fire-and-forget; poll briefly for it to land.
	deadline := time.Now().Add(2 * time.Second)
	got := 1.0
	for time.Now().Before(deadline) {
		got = s.lcr.Snapshot(context.Background(), []lcr.Key{key})[key]
		if got < 1.0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got >= 1.0 {
		t.Fatalf("availability after a recorded failure = %v, want < 1.0 (the EWMA must actually move)", got)
	}
}
