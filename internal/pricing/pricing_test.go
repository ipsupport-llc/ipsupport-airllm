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

func TestAudioCostMicroUSD(t *testing.T) {
	tab := New()
	// $0.006/minute == $0.0001/second == $100 per 1M seconds.
	tab.Set("groq", "whisper-large-v3", Price{InputPer1M: 100, Unit: "audio_second"})

	got := tab.AudioCostMicroUSD("groq", "whisper-large-v3", 30) // 30s clip
	want := int64(3000)                                          // 30/1e6*100 USD = 0.003 USD = 3000 microUSD
	if got != want {
		t.Errorf("AudioCostMicroUSD = %d, want %d", got, want)
	}

	if got := tab.AudioCostMicroUSD("groq", "unknown-model", 30); got != 0 {
		t.Errorf("unknown model = %d, want 0", got)
	}
}

func TestTTSCostMicroUSD(t *testing.T) {
	tab := New()
	// OpenAI's own convention: $15.00 / 1M characters.
	tab.Set("openai", "tts-1", Price{InputPer1M: 15, Unit: "text_char"})

	got := tab.TTSCostMicroUSD("openai", "tts-1", 100) // 100-char input
	want := int64(1500)                                // 100/1e6*15 USD = 0.0015 USD = 1500 microUSD
	if got != want {
		t.Errorf("TTSCostMicroUSD = %d, want %d", got, want)
	}
}

func TestPriceUnitRoundTrip(t *testing.T) {
	tab := New()
	tab.Set("groq", "m", Price{InputPer1M: 5, OutputPer1M: 0, Unit: "audio_second"})
	// Set/lookup round-trips Unit alongside the existing fields — a caller
	// using the wrong cost method for a row's unit is a caller bug, not
	// something the table cross-checks (documented in the design spec).
	got := tab.AudioCostMicroUSD("groq", "m", 1e6)
	if got != 5_000_000 {
		t.Errorf("got %d, want 5000000", got)
	}
}
