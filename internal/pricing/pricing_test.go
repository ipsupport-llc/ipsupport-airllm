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

// --- Context-tiered pricing ---------------------------------------------
//
// Gemini 2.5 Pro's real published rates: $1.25 / $10.00 per 1M tokens for a
// prompt up to 200 000 tokens, $2.50 / $15.00 above it. The threshold is read
// off the PROMPT count, and both rates switch together — the output of a long
// prompt is billed at the high output rate too.

func geminiPro() *Table {
	tab := New()
	tab.Set("vertex", "google/gemini-2.5-pro", Price{
		InputPer1M: 1.25, OutputPer1M: 10,
		ContextThreshold: 200_000,
		InputPer1MAbove:  2.50, OutputPer1MAbove: 15,
	})
	return tab
}

func TestCostMicroUSD_AboveThresholdUsesTheHighRates(t *testing.T) {
	// 300k prompt is over the 200k breakpoint: 300000/1e6*2.50 = $0.75 input,
	// 1000/1e6*15 = $0.015 output. Both rates switch, not just input.
	got := geminiPro().CostMicroUSD("vertex", "google/gemini-2.5-pro", 300_000, 1000)
	want := int64(765_000)
	if got != want {
		t.Errorf("above threshold = %d, want %d", got, want)
	}
}

func TestCostMicroUSD_AtOrBelowThresholdUsesTheBaseRates(t *testing.T) {
	// The vendor's tier reads "up to 200 000 tokens", so the breakpoint itself
	// is still the low rate and only a strictly larger prompt crosses over.
	tab := geminiPro()
	for _, tc := range []struct {
		name   string
		prompt int
		want   int64
	}{
		// 100000/1e6*1.25 = $0.125 + 1000/1e6*10 = $0.01
		{"below", 100_000, 135_000},
		// 200000/1e6*1.25 = $0.25 + $0.01
		{"exactly at the breakpoint", 200_000, 260_000},
		// One token past it: 200001/1e6*2.50 = $0.5000025 + 1000/1e6*15
		{"one token above", 200_001, 515_003},
	} {
		if got := tab.CostMicroUSD("vertex", "google/gemini-2.5-pro", tc.prompt, 1000); got != tc.want {
			t.Errorf("%s: = %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestCostMicroUSD_UntieredRowIsPricedExactlyAsBefore(t *testing.T) {
	// A row with no threshold ignores the second pair of rates entirely, even
	// when they carry values — a threshold of 0 is the "no tier" marker, and
	// no prompt size can make it apply.
	tab := New()
	tab.Set("vertex", "google/gemini-2.5-flash", Price{
		InputPer1M: 0.30, OutputPer1M: 2.50,
		InputPer1MAbove: 99, OutputPer1MAbove: 99,
	})
	// 10_000_000/1e6*0.30 = $3.00 + 1000/1e6*2.50 = $0.0025
	got := tab.CostMicroUSD("vertex", "google/gemini-2.5-flash", 10_000_000, 1000)
	if want := int64(3_002_500); got != want {
		t.Errorf("untiered huge prompt = %d, want %d", got, want)
	}
}

func TestCostMicroUSD_TierFollowsTheRowThatWon(t *testing.T) {
	// The tier belongs to the row the lookup picked, so an exact provider row
	// without a tier is not given the wildcard row's one.
	tab := New()
	tab.Set("", "m", Price{
		InputPer1M: 1, OutputPer1M: 1,
		ContextThreshold: 10, InputPer1MAbove: 100, OutputPer1MAbove: 100,
	})
	tab.Set("vertex", "m", Price{InputPer1M: 1, OutputPer1M: 1})

	if got := tab.CostMicroUSD("vertex", "m", 1_000_000, 0); got != 1_000_000 {
		t.Errorf("exact untiered row = %d, want 1000000", got)
	}
	if got := tab.CostMicroUSD("other", "m", 1_000_000, 0); got != 100_000_000 {
		t.Errorf("wildcard tiered row = %d, want 100000000", got)
	}
}

func TestPriceValidate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		p       Price
		wantErr bool
	}{
		{"untiered token row", Price{InputPer1M: 1, OutputPer1M: 2, Unit: "tokens"}, false},
		{"complete tier", Price{InputPer1M: 1, OutputPer1M: 2, Unit: "tokens",
			ContextThreshold: 200_000, InputPer1MAbove: 2, OutputPer1MAbove: 4}, false},
		{"negative threshold", Price{Unit: "tokens", ContextThreshold: -1}, true},
		{"negative rate", Price{InputPer1M: -1, Unit: "tokens"}, true},
		// The whole point of a tier is a second rate; leaving one at zero
		// would price a long prompt at nothing, which is worse than the flat
		// rate it replaced.
		{"tier missing its input rate", Price{InputPer1M: 1, OutputPer1M: 2, Unit: "tokens",
			ContextThreshold: 200_000, OutputPer1MAbove: 4}, true},
		{"tier missing its output rate", Price{InputPer1M: 1, OutputPer1M: 2, Unit: "tokens",
			ContextThreshold: 200_000, InputPer1MAbove: 2}, true},
		// Audio and TTS rows are priced by duration and characters; a prompt
		// token threshold is never consulted for them, so accepting one would
		// store a setting that silently does nothing.
		{"tier on an audio row", Price{InputPer1M: 1, Unit: "audio_second",
			ContextThreshold: 200_000, InputPer1MAbove: 2, OutputPer1MAbove: 4}, true},
		{"unknown unit", Price{InputPer1M: 1, Unit: "bananas"}, true},
	} {
		err := tc.p.Validate()
		if (err != nil) != tc.wantErr {
			t.Errorf("%s: Validate() = %v, wantErr %v", tc.name, err, tc.wantErr)
		}
	}
}
