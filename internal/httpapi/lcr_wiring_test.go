package httpapi

import (
	"math"
	"testing"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/lcr"
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
