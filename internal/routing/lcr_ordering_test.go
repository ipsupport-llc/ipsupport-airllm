package routing

import (
	"math"
	"testing"
)

func lcrTarget(provider string) Target { return Target{Provider: provider, Tier: 0} }

// TestOrderTierLCRSingleTargetIsNoOp covers the actual shape of the spec's
// worked examples (a tier holding exactly one pinned local target) — it must
// return trivially, never call rank, never panic on an empty min/max or
// partition.
func TestOrderTierLCRSingleTargetIsNoOp(t *testing.T) {
	tier := []Target{lcrTarget("solo")}
	out := orderTierLCR(tier, "both", 0.5, 0, func(Target) LCRRank {
		t.Fatal("rank must not be called for a single-target tier")
		return LCRRank{}
	})
	if len(out) != 1 || out[0].Provider != "solo" {
		t.Fatalf("orderTierLCR on a single-target tier = %v, want the same single target unchanged", out)
	}
}

func TestOrderTierLCRPriceMode(t *testing.T) {
	tier := []Target{lcrTarget("expensive"), lcrTarget("cheap"), lcrTarget("mid")}
	rank := func(tgt Target) LCRRank {
		switch tgt.Provider {
		case "cheap":
			return LCRRank{Price: 1.0, Availability: 1.0}
		case "mid":
			return LCRRank{Price: 5.0, Availability: 1.0}
		default:
			return LCRRank{Price: 100.0, Availability: 1.0}
		}
	}
	out := orderTierLCR(tier, "price", 0, 0, rank)
	got := []string{out[0].Provider, out[1].Provider, out[2].Provider}
	want := []string{"cheap", "mid", "expensive"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

func TestOrderTierLCRPriceModeUnpricedSortsLast(t *testing.T) {
	tier := []Target{lcrTarget("priced"), lcrTarget("unpriced")}
	rank := func(tgt Target) LCRRank {
		if tgt.Provider == "unpriced" {
			return LCRRank{Price: math.Inf(1), Availability: 1.0}
		}
		return LCRRank{Price: 2.0, Availability: 1.0}
	}
	out := orderTierLCR(tier, "price", 0, 0, rank)
	if out[0].Provider != "priced" || out[1].Provider != "unpriced" {
		t.Fatalf("order = %v, want [priced, unpriced]", []string{out[0].Provider, out[1].Provider})
	}
}

func TestOrderTierLCRAvailabilityMode(t *testing.T) {
	tier := []Target{lcrTarget("flaky"), lcrTarget("solid")}
	rank := func(tgt Target) LCRRank {
		if tgt.Provider == "flaky" {
			return LCRRank{Price: 1.0, Availability: 0.3}
		}
		return LCRRank{Price: 1.0, Availability: 0.95}
	}
	out := orderTierLCR(tier, "availability", 0, 0, rank)
	if out[0].Provider != "solid" {
		t.Fatalf("order = %v, want solid first (higher availability)", []string{out[0].Provider, out[1].Provider})
	}
}

func TestOrderTierLCRBothModeThresholdGate(t *testing.T) {
	tier := []Target{lcrTarget("cheap-but-flaky"), lcrTarget("pricier-but-healthy")}
	rank := func(tgt Target) LCRRank {
		if tgt.Provider == "cheap-but-flaky" {
			return LCRRank{Price: 1.0, Availability: 0.2} // below the 0.5 threshold
		}
		return LCRRank{Price: 10.0, Availability: 0.9}
	}
	out := orderTierLCR(tier, "both", 0.5, 0, rank)
	if out[0].Provider != "pricier-but-healthy" {
		t.Fatalf("order = %v, want the above-threshold target first even though it's pricier", []string{out[0].Provider, out[1].Provider})
	}
}

func TestOrderTierLCRBothModeSortsByPriceWithinEachGroup(t *testing.T) {
	tier := []Target{lcrTarget("a"), lcrTarget("b"), lcrTarget("c")}
	rank := func(tgt Target) LCRRank {
		switch tgt.Provider {
		case "a":
			return LCRRank{Price: 5.0, Availability: 0.9} // above threshold, pricier
		case "b":
			return LCRRank{Price: 1.0, Availability: 0.9} // above threshold, cheaper
		default:
			return LCRRank{Price: 0.5, Availability: 0.1} // below threshold, cheapest overall
		}
	}
	out := orderTierLCR(tier, "both", 0.5, 0, rank)
	got := []string{out[0].Provider, out[1].Provider, out[2].Provider}
	want := []string{"b", "a", "c"} // above-threshold group by price (b, a), then below-threshold (c)
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

func TestOrderTierLCRTieBreakRotatesOnlyTiedGroup(t *testing.T) {
	// A and B tie on price; C is strictly cheaper and must always be first.
	// Across 3 ticks of rr, A and B must each lead exactly once between
	// themselves — rotating the WHOLE tier first (a caught bug in an
	// earlier draft) would instead let an untied member shift their
	// relative rotation unevenly.
	tier := []Target{lcrTarget("a"), lcrTarget("b"), lcrTarget("c")}
	rank := func(tgt Target) LCRRank {
		if tgt.Provider == "c" {
			return LCRRank{Price: 0.5, Availability: 1.0}
		}
		return LCRRank{Price: 1.0, Availability: 1.0} // a and b tie
	}
	firsts := map[string]int{}
	for rr := uint64(0); rr < 4; rr++ {
		out := orderTierLCR(tier, "price", 0, rr, rank)
		if out[0].Provider != "c" {
			t.Fatalf("rr=%d: c (strictly cheapest) must always be first, got %v", rr, out[0].Provider)
		}
		firsts[out[1].Provider]++
	}
	if firsts["a"] == 0 || firsts["b"] == 0 {
		t.Errorf("tied group [a,b] never rotated across 4 ticks: %v", firsts)
	}
}

func TestPlanOrderedPassesThroughToLCRWhenStrategyIsLCR(t *testing.T) {
	p := &Plan{Strategy: "lcr", LCRMode: "price", Tiers: [][]Target{{lcrTarget("expensive"), lcrTarget("cheap")}}}
	rank := func(tgt Target) LCRRank {
		if tgt.Provider == "cheap" {
			return LCRRank{Price: 1.0, Availability: 1.0}
		}
		return LCRRank{Price: 9.0, Availability: 1.0}
	}
	out := p.Ordered(0, nil, rank)
	if out[0].Provider != "cheap" {
		t.Fatalf("Plan.Ordered with strategy=lcr = %v, want cheap first", []string{out[0].Provider, out[1].Provider})
	}
}

func TestPlanOrderedIgnoresRankForNonLCRStrategy(t *testing.T) {
	// A nil free func is fine here because round_robin never calls it.
	p := &Plan{Strategy: "round_robin", Tiers: [][]Target{{lcrTarget("a"), lcrTarget("b")}}}
	out := p.Ordered(0, nil, func(Target) LCRRank { t.Fatal("rank must not be called for a non-lcr strategy"); return LCRRank{} })
	if len(out) != 2 {
		t.Fatalf("got %d targets, want 2", len(out))
	}
}
