package pricing

import "testing"

func TestCostMicroUSD_ExactBeatsWildcard(t *testing.T) {
	tab := New()
	tab.Set("openrouter", "m", Price{InputPer1M: 1, OutputPer1M: 2})
	tab.Set("", "m", Price{InputPer1M: 10, OutputPer1M: 20})

	// Exact provider+model match wins over the wildcard row.
	got := tab.CostMicroUSD("openrouter", "m", 1000, 1000)
	want := int64(3000) // 1000/1e6*1 + 1000/1e6*2 = 0.003 USD = 3000 microUSD
	if got != want {
		t.Errorf("CostMicroUSD(openrouter, m) = %d, want %d", got, want)
	}
}

func TestCostMicroUSD_WildcardFallback(t *testing.T) {
	tab := New()
	tab.Set("openrouter", "m", Price{InputPer1M: 1, OutputPer1M: 2})
	tab.Set("", "m", Price{InputPer1M: 10, OutputPer1M: 20})

	// A different provider than the exact row falls back to the wildcard.
	got := tab.CostMicroUSD("anotherprovider", "m", 1000, 1000)
	want := int64(30000) // 1000/1e6*10 + 1000/1e6*20 = 0.03 USD = 30000 microUSD
	if got != want {
		t.Errorf("CostMicroUSD(anotherprovider, m) = %d, want %d", got, want)
	}
}

func TestCostMicroUSD_UnknownModelIsZero(t *testing.T) {
	tab := New()
	tab.Set("openrouter", "m", Price{InputPer1M: 1, OutputPer1M: 2})
	tab.Set("", "m", Price{InputPer1M: 10, OutputPer1M: 20})

	got := tab.CostMicroUSD("openrouter", "x", 1000, 1000)
	if got != 0 {
		t.Errorf("CostMicroUSD(openrouter, x) = %d, want 0", got)
	}
}
