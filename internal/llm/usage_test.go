package llm

import (
	"encoding/json"
	"testing"
)

// TestUsageNormalize pins the invariant every downstream consumer relies on:
// CompletionTokens is everything the vendor bills at the output rate,
// ReasoningTokens is the thinking share of that, and TotalTokens is the sum of
// the parts.
func TestUsageNormalize(t *testing.T) {
	cases := []struct {
		name string
		in   Usage
		want Usage
	}{
		{
			// The overwhelmingly common case: no reasoning anywhere, parts sum
			// to the total. Nothing may move.
			name: "consistent report without reasoning is untouched",
			in:   Usage{PromptTokens: 7, CompletionTokens: 3, TotalTokens: 10},
			want: Usage{PromptTokens: 7, CompletionTokens: 3, TotalTokens: 10},
		},
		{
			// The Vertex AI shape, measured live on 2026-09-04: 10 + 30 is not
			// 186, and the missing 146 are thinking tokens Google bills at the
			// output rate. Dropping the difference under-reported that call
			// 5.7x. This is the case ticket 10 exists for.
			name: "parts short of the total fold the difference into completion",
			in:   Usage{PromptTokens: 10, CompletionTokens: 30, TotalTokens: 186},
			want: Usage{PromptTokens: 10, CompletionTokens: 176, TotalTokens: 186, ReasoningTokens: 146},
		},
		{
			// The OpenAI shape: reasoning is already inside completion_tokens
			// and completion_tokens_details only breaks it out. Folding it in
			// again would double-count it.
			name: "reasoning already inside completion is a breakdown, not an addition",
			in:   Usage{PromptTokens: 10, CompletionTokens: 176, TotalTokens: 186, ReasoningTokens: 146},
			want: Usage{PromptTokens: 10, CompletionTokens: 176, TotalTokens: 186, ReasoningTokens: 146},
		},
		{
			// A vendor that both names the count and leaves it out of
			// completion_tokens: the two agree, and the result must match the
			// two cases above rather than counting 146 twice.
			name: "reasoning named and excluded from completion is folded once",
			in:   Usage{PromptTokens: 10, CompletionTokens: 30, TotalTokens: 186, ReasoningTokens: 146},
			want: Usage{PromptTokens: 10, CompletionTokens: 176, TotalTokens: 186, ReasoningTokens: 146},
		},
		{
			// Whatever the vendor reported, the gap is output it billed, so
			// the gap is the floor for the reasoning count.
			name: "the gap wins over a smaller named reasoning count",
			in:   Usage{PromptTokens: 10, CompletionTokens: 30, TotalTokens: 186, ReasoningTokens: 100},
			want: Usage{PromptTokens: 10, CompletionTokens: 176, TotalTokens: 186, ReasoningTokens: 146},
		},
		{
			// Reasoning is billed as output, so completion has to cover it
			// even when the vendor's own total does not.
			name: "reasoning larger than completion raises completion and the total",
			in:   Usage{PromptTokens: 10, CompletionTokens: 0, TotalTokens: 10, ReasoningTokens: 146},
			want: Usage{PromptTokens: 10, CompletionTokens: 146, TotalTokens: 156, ReasoningTokens: 146},
		},
		{
			name: "a missing total is computed from the parts",
			in:   Usage{PromptTokens: 7, CompletionTokens: 3},
			want: Usage{PromptTokens: 7, CompletionTokens: 3, TotalTokens: 10},
		},
		{
			// A total larger than the parts is the signal; a total smaller
			// than them is a vendor slip, and correcting the total must not
			// invent output tokens to charge for.
			name: "a total below the parts is corrected without touching them",
			in:   Usage{PromptTokens: 7, CompletionTokens: 3, TotalTokens: 4},
			want: Usage{PromptTokens: 7, CompletionTokens: 3, TotalTokens: 10},
		},
		{
			name: "an empty report stays empty",
			in:   Usage{},
			want: Usage{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.in
			got.Normalize()
			if got != tc.want {
				t.Errorf("Normalize()\n got: %+v\nwant: %+v", got, tc.want)
			}
			// Normalizing an already-normalized report must be a no-op:
			// usage crosses several decode layers and may be normalized more
			// than once on the way through.
			again := got
			again.Normalize()
			if again != got {
				t.Errorf("Normalize() is not idempotent\n once: %+v\ntwice: %+v", got, again)
			}
		})
	}
}

// TestUsageUnmarshalReadsReasoningAndNormalizes proves the two vendor
// spellings both arrive as the same normalized IR, straight off the wire —
// the decode every provider shares, so no provider carries its own fix.
func TestUsageUnmarshalReadsReasoningAndNormalizes(t *testing.T) {
	cases := []struct {
		name string
		body string
		want Usage
	}{
		{
			name: "openai completion_tokens_details",
			body: `{"prompt_tokens":10,"completion_tokens":176,"total_tokens":186,
			        "completion_tokens_details":{"reasoning_tokens":146}}`,
			want: Usage{PromptTokens: 10, CompletionTokens: 176, TotalTokens: 186, ReasoningTokens: 146},
		},
		{
			// The payload ticket 10 asks to be pinned: the parts do not sum to
			// the total and the vendor names no reasoning field at all. If the
			// difference is ever dropped again, this fails.
			name: "vertex reports it only inside total_tokens",
			body: `{"prompt_tokens":10,"completion_tokens":30,"total_tokens":186}`,
			want: Usage{PromptTokens: 10, CompletionTokens: 176, TotalTokens: 186, ReasoningTokens: 146},
		},
		{
			name: "no reasoning anywhere",
			body: `{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}`,
			want: Usage{PromptTokens: 7, CompletionTokens: 3, TotalTokens: 10},
		},
		{
			name: "a null usage object decodes to nothing",
			body: `null`,
			want: Usage{},
		},
		{
			name: "a details object without a reasoning count",
			body: `{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10,"completion_tokens_details":{}}`,
			want: Usage{PromptTokens: 7, CompletionTokens: 3, TotalTokens: 10},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got Usage
			if err := json.Unmarshal([]byte(tc.body), &got); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if got != tc.want {
				t.Errorf("decoded\n got: %+v\nwant: %+v", got, tc.want)
			}
		})
	}
}

func TestUsageUnmarshalRejectsMalformedJSON(t *testing.T) {
	var u Usage
	if err := json.Unmarshal([]byte(`{"prompt_tokens":"lots"}`), &u); err == nil {
		t.Fatal("want an error for a non-numeric token count, got nil")
	}
}

// TestUsageMarshal pins the client-facing wire shape. The zero-reasoning case
// must be byte-identical to what the gateway emitted before reasoning was
// accounted for: no new field may appear from nowhere.
func TestUsageMarshal(t *testing.T) {
	cases := []struct {
		name string
		in   Usage
		want string
	}{
		{
			name: "without reasoning, exactly the three fields as before",
			in:   Usage{PromptTokens: 7, CompletionTokens: 3, TotalTokens: 10},
			want: `{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}`,
		},
		{
			name: "with reasoning, in OpenAI's own place for it",
			in:   Usage{PromptTokens: 10, CompletionTokens: 176, TotalTokens: 186, ReasoningTokens: 146},
			want: `{"prompt_tokens":10,"completion_tokens":176,"total_tokens":186,"completion_tokens_details":{"reasoning_tokens":146}}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, err := json.Marshal(tc.in)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if string(b) != tc.want {
				t.Errorf("marshalled\n got: %s\nwant: %s", b, tc.want)
			}
		})
	}
}

// TestUsageUnmarshalNullLeavesTheReceiver pins the stdlib convention that a
// JSON null is a no-op. It matters here because UnmarshalJSON replaces the
// whole value: an upstream that sends "usage": null on a later chunk must not
// wipe counts a previous one already put in the receiver.
func TestUsageUnmarshalNullLeavesTheReceiver(t *testing.T) {
	got := Usage{PromptTokens: 10, CompletionTokens: 176, TotalTokens: 186, ReasoningTokens: 146}
	before := got
	if err := json.Unmarshal([]byte("null"), &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got != before {
		t.Errorf("null wiped the receiver\n got: %+v\nwant: %+v", got, before)
	}
}
