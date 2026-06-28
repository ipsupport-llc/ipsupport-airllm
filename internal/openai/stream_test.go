package openai

import (
	"strings"
	"testing"

	"github.com/rromenskyi/ipsupport-airllm/internal/llm"
)

func mustMarshal(t *testing.T, c llm.StreamChunk) string {
	t.Helper()
	b, err := MarshalStreamChunk(StreamMeta{ID: "id1", Model: "m1", Created: 7}, c)
	if err != nil {
		t.Fatalf("MarshalStreamChunk: %v", err)
	}
	return string(b)
}

func TestStreamChunkContent(t *testing.T) {
	s := mustMarshal(t, llm.StreamChunk{Role: "assistant", Content: "hi"})
	for _, want := range []string{
		`"object":"chat.completion.chunk"`,
		`"role":"assistant"`,
		`"content":"hi"`,
		`"finish_reason":null`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("content chunk missing %s in %s", want, s)
		}
	}
}

func TestStreamChunkFinish(t *testing.T) {
	s := mustMarshal(t, llm.StreamChunk{FinishReason: "stop"})
	if !strings.Contains(s, `"finish_reason":"stop"`) {
		t.Errorf("finish chunk missing finish_reason: %s", s)
	}
}

func TestStreamChunkUsageOnly(t *testing.T) {
	s := mustMarshal(t, llm.StreamChunk{Usage: &llm.Usage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3}})
	if !strings.Contains(s, `"choices":[]`) {
		t.Errorf("usage chunk should carry empty choices: %s", s)
	}
	if !strings.Contains(s, `"usage":{`) {
		t.Errorf("usage chunk missing usage object: %s", s)
	}
}

func TestStreamChunkToolCall(t *testing.T) {
	s := mustMarshal(t, llm.StreamChunk{ToolCalls: []llm.ToolCall{{
		ID: "call_1", Type: "function",
		Function: llm.FunctionCall{Name: "do_thing", Arguments: "{}"},
	}}})
	for _, want := range []string{`"tool_calls"`, `"name":"do_thing"`, `"index":0`} {
		if !strings.Contains(s, want) {
			t.Errorf("tool-call chunk missing %s in %s", want, s)
		}
	}
}
