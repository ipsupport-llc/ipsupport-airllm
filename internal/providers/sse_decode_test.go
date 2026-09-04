package providers

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/llm"
)

// sseBody formats raw JSON payloads (plus a literal "[DONE]" when the test
// wants one) as an SSE response body, exactly as an upstream writes it.
func sseBody(payloads ...string) string {
	var b strings.Builder
	for _, p := range payloads {
		b.WriteString("data: " + p + "\n\n")
	}
	return b.String()
}

func decodeAll(t *testing.T, body string, opts streamDecodeOptions) []llm.StreamChunk {
	t.Helper()
	var got []llm.StreamChunk
	err := decodeSSEStream(strings.NewReader(body), opts, func(c llm.StreamChunk) error {
		got = append(got, c)
		return nil
	})
	if err != nil {
		t.Fatalf("decodeSSEStream: %v", err)
	}
	return got
}

func usage(prompt, completion, total int) *llm.Usage {
	return &llm.Usage{PromptTokens: prompt, CompletionTokens: completion, TotalTokens: total}
}

// TestDecodeSSEStreamNonCoalescingOutput pins the exact chunk sequence the
// non-coalescing path produces for every upstream shape the gateway has met
// in the wild. It asserts whole chunks, not selected fields, so any drift in
// the extracted decoder — a reordered usage chunk, a stray empty chunk, a
// finish reason that stops being synthesized — fails here.
func TestDecodeSSEStreamNonCoalescingOutput(t *testing.T) {
	toolCall := []llm.ToolCallDelta{{
		Index:    0,
		ID:       "call_1",
		Type:     "function",
		Function: llm.FunctionCall{Name: "file", Arguments: `{"a":1}`},
	}}

	cases := []struct {
		name     string
		payloads []string
		want     []llm.StreamChunk
	}{
		{
			name: "groq tool call with usage-only chunk and no finish_reason anywhere",
			payloads: []string{
				`{"choices":[{"delta":{"role":"assistant"},"finish_reason":null}]}`,
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"file","arguments":"{\"a\":1}"}}]},"finish_reason":null}]}`,
				`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
				"[DONE]",
			},
			want: []llm.StreamChunk{
				{Role: "assistant"},
				{ToolCalls: toolCall},
				{FinishReason: "tool_calls"},
				{Usage: usage(10, 5, 15)},
			},
		},
		{
			name: "plain text with no finish_reason gets a synthesized stop",
			payloads: []string{
				`{"choices":[{"delta":{"role":"assistant"},"finish_reason":null}]}`,
				`{"choices":[{"delta":{"content":"hi"},"finish_reason":null}]}`,
				`{"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`,
				"[DONE]",
			},
			want: []llm.StreamChunk{
				{Role: "assistant"},
				{Content: "hi"},
				{FinishReason: "stop"},
				{Usage: usage(3, 1, 4)},
			},
		},
		{
			name: "well-behaved upstream gets no extra synthesized chunk",
			payloads: []string{
				`{"choices":[{"delta":{"content":"hi"},"finish_reason":null}]}`,
				`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
				`{"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`,
				"[DONE]",
			},
			want: []llm.StreamChunk{
				{Content: "hi"},
				{FinishReason: "stop"},
				{Usage: usage(3, 1, 4)},
			},
		},
		{
			name: "groq bundles finish_reason and usage in one object; they split apart",
			payloads: []string{
				`{"choices":[{"delta":{"role":"assistant"},"finish_reason":null}]}`,
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"file","arguments":"{\"a\":1}"}}]},"finish_reason":null}]}`,
				`{"choices":[{"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":291,"completion_tokens":246,"total_tokens":537}}`,
				"[DONE]",
			},
			want: []llm.StreamChunk{
				{Role: "assistant"},
				{ToolCalls: toolCall},
				{FinishReason: "tool_calls"},
				{Usage: usage(291, 246, 537)},
			},
		},
		{
			name: "upstream bundles the finish reason onto a content delta; they split apart",
			payloads: []string{
				`{"choices":[{"delta":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`,
				`{"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`,
				"[DONE]",
			},
			want: []llm.StreamChunk{
				{Role: "assistant", Content: "hi"},
				{FinishReason: "stop"},
				{Usage: usage(3, 1, 4)},
			},
		},
		{
			name: "a finish reason bundled onto a tool-call delta splits too",
			payloads: []string{
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"file","arguments":"{\"a\":1}"}}]},"finish_reason":"tool_calls"}]}`,
				"[DONE]",
			},
			want: []llm.StreamChunk{
				{ToolCalls: toolCall},
				{FinishReason: "tool_calls"},
			},
		},
		{
			name: "no usage chunk at all still ends with a finish signal",
			payloads: []string{
				`{"choices":[{"delta":{"content":"hi"},"finish_reason":null}]}`,
				"[DONE]",
			},
			want: []llm.StreamChunk{
				{Content: "hi"},
				{FinishReason: "stop"},
			},
		},
		{
			name: "stream that just stops, with no [DONE], still gets a finish signal",
			payloads: []string{
				`{"choices":[{"delta":{"content":"hi"},"finish_reason":null}]}`,
			},
			want: []llm.StreamChunk{
				{Content: "hi"},
				{FinishReason: "stop"},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := decodeAll(t, sseBody(tc.payloads...), streamDecodeOptions{provider: "up"})
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("chunks drifted\n got: %+v\nwant: %+v", got, tc.want)
			}
		})
	}
}

// TestDecodeSSEStreamCoalescesCumulativeUsage is the Vertex AI shape: usage
// repeats on nearly every chunk and grows as the response is generated. The
// client must see one usage report, holding the last totals, after the finish.
func TestDecodeSSEStreamCoalescesCumulativeUsage(t *testing.T) {
	body := sseBody(
		`{"choices":[{"delta":{"role":"assistant","content":"he"},"finish_reason":null}],"usage":{"prompt_tokens":7,"completion_tokens":1,"total_tokens":8}}`,
		`{"choices":[{"delta":{"content":"llo"},"finish_reason":null}],"usage":{"prompt_tokens":7,"completion_tokens":2,"total_tokens":9}}`,
		`{"choices":[{"delta":{"content":"!"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`,
		"[DONE]",
	)

	got := decodeAll(t, body, streamDecodeOptions{provider: "vertex", coalesceUsage: true})
	// Vertex's last chunk carries all three of content, finish_reason and
	// usage. All three come apart: this is the shape observed live against
	// the real service, so it is the shape the contract is pinned against.
	want := []llm.StreamChunk{
		{Role: "assistant", Content: "he"},
		{Content: "llo"},
		{Content: "!"},
		{FinishReason: "stop"},
		{Usage: usage(7, 3, 10)},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("coalesced stream wrong\n got: %+v\nwant: %+v", got, want)
	}
}

// A coalescing upstream that never sends a finish_reason must still get one,
// and it must land before the single usage report.
func TestDecodeSSEStreamCoalescingSynthesizesFinishBeforeUsage(t *testing.T) {
	body := sseBody(
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"file","arguments":"{}"}}]},"finish_reason":null}],"usage":{"prompt_tokens":4,"completion_tokens":1,"total_tokens":5}}`,
		`{"choices":[],"usage":{"prompt_tokens":4,"completion_tokens":9,"total_tokens":13}}`,
		"[DONE]",
	)

	got := decodeAll(t, body, streamDecodeOptions{provider: "vertex", coalesceUsage: true})
	want := []llm.StreamChunk{
		{ToolCalls: []llm.ToolCallDelta{{Index: 0, ID: "call_1", Type: "function", Function: llm.FunctionCall{Name: "file", Arguments: "{}"}}}},
		{FinishReason: "tool_calls"},
		{Usage: usage(4, 9, 13)},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("coalesced stream wrong\n got: %+v\nwant: %+v", got, want)
	}
}

// Coalescing changes when usage is reported, not whether it is: a stream that
// carries none must not gain an empty usage chunk.
func TestDecodeSSEStreamCoalescingEmitsNoUsageWhenUpstreamSendsNone(t *testing.T) {
	body := sseBody(
		`{"choices":[{"delta":{"content":"hi"},"finish_reason":"stop"}]}`,
		"[DONE]",
	)

	got := decodeAll(t, body, streamDecodeOptions{provider: "vertex", coalesceUsage: true})
	want := []llm.StreamChunk{{Content: "hi"}, {FinishReason: "stop"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got: %+v, want: %+v", got, want)
	}
}

func TestDecodeSSEStreamSkipsMalformedChunksAndNonDataLines(t *testing.T) {
	body := "event: message\n" +
		"data: {not json\n\n" +
		"data:\n\n" +
		": keepalive\n\n" +
		`data: {"choices":[{"delta":{"content":"hi"},"finish_reason":"stop"}]}` + "\n\n" +
		"data: [DONE]\n\n"

	got := decodeAll(t, body, streamDecodeOptions{provider: "up"})
	want := []llm.StreamChunk{{Content: "hi"}, {FinishReason: "stop"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got: %+v, want: %+v", got, want)
	}
}

func TestDecodeSSEStreamStopsOnYieldError(t *testing.T) {
	boom := errors.New("client gone")
	body := sseBody(
		`{"choices":[{"delta":{"content":"a"},"finish_reason":null}]}`,
		`{"choices":[{"delta":{"content":"b"},"finish_reason":null}]}`,
		"[DONE]",
	)

	var n int
	err := decodeSSEStream(strings.NewReader(body), streamDecodeOptions{provider: "up"}, func(llm.StreamChunk) error {
		n++
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
	if n != 1 {
		t.Errorf("yield called %d times, want 1 — decoding must stop at the first error", n)
	}
}

// TestDecodeSSEStreamKeepsReasoningTokens pins the payload whose parts do not
// sum to its total. The difference is thinking the vendor billed and reported
// nowhere else, so it must survive the decode — under both coalescing modes,
// because the fix belongs to the shared loop and not to one provider.
func TestDecodeSSEStreamKeepsReasoningTokens(t *testing.T) {
	// Vertex's live shape: cumulative usage on every chunk, the last of which
	// reports 10 + 30 against a total of 186.
	body := sseBody(
		`{"choices":[{"delta":{"role":"assistant","content":"o"}}],"usage":{"prompt_tokens":10,"completion_tokens":0,"total_tokens":98}}`,
		`{"choices":[{"delta":{"content":"k"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":30,"total_tokens":186}}`,
		"[DONE]",
	)
	want := &llm.Usage{PromptTokens: 10, CompletionTokens: 176, TotalTokens: 186, ReasoningTokens: 146}

	for _, coalesce := range []bool{true, false} {
		t.Run(fmt.Sprintf("coalesceUsage=%v", coalesce), func(t *testing.T) {
			got := decodeAll(t, body, streamDecodeOptions{provider: "vertex", coalesceUsage: coalesce})
			last := got[len(got)-1]
			if last.Usage == nil {
				t.Fatalf("the stream ended without a usage chunk: %+v", got)
			}
			if *last.Usage != *want {
				t.Errorf("final usage = %+v, want %+v — the 146 tokens between the parts and the total were dropped",
					*last.Usage, *want)
			}
		})
	}
}

// The reasoning count OpenAI-shaped upstreams spell out is read too, and is
// NOT added to a completion count that already contains it.
func TestDecodeSSEStreamReadsCompletionTokensDetails(t *testing.T) {
	body := sseBody(
		`{"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":176,"total_tokens":186,"completion_tokens_details":{"reasoning_tokens":146}}}`,
		"[DONE]",
	)

	got := decodeAll(t, body, streamDecodeOptions{provider: "openai"})
	want := []llm.StreamChunk{
		{Content: "ok"},
		{FinishReason: "stop"},
		{Usage: &llm.Usage{PromptTokens: 10, CompletionTokens: 176, TotalTokens: 186, ReasoningTokens: 146}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("stream wrong\n got: %+v\nwant: %+v", got, want)
	}
}
