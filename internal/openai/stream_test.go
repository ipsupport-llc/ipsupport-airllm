package openai

import (
	"strings"
	"testing"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/llm"
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
	s := mustMarshal(t, llm.StreamChunk{ToolCalls: []llm.ToolCallDelta{{
		ID: "call_1", Type: "function",
		Function: llm.FunctionCall{Name: "do_thing", Arguments: "{}"},
	}}})
	for _, want := range []string{`"tool_calls"`, `"name":"do_thing"`, `"index":0`} {
		if !strings.Contains(s, want) {
			t.Errorf("tool-call chunk missing %s in %s", want, s)
		}
	}
}

func TestMarshalStreamChunkPreservesToolCallIndex(t *testing.T) {
	meta := StreamMeta{ID: "id1", Model: "m", Created: 1}
	b, err := MarshalStreamChunk(meta, llm.StreamChunk{ToolCalls: []llm.ToolCallDelta{{
		Index: 1, ID: "call_2", Type: "function",
		Function: llm.FunctionCall{Name: "file", Arguments: `{"b":2}`},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"index":1`) {
		t.Errorf("upstream index lost: %s", b)
	}
}

func TestParseStreamChunkKeepsIndexAndObjectArgs(t *testing.T) {
	// grok shape: two parallel calls, whole arguments per delta, indexes 0/1
	c1, err := ParseStreamChunk([]byte(`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_2","type":"function","function":{"name":"file","arguments":"{\"b\":2}"}}]},"finish_reason":null}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(c1.ToolCalls) != 1 || c1.ToolCalls[0].Index != 1 || c1.ToolCalls[0].Function.Arguments != `{"b":2}` {
		t.Errorf("got %+v", c1.ToolCalls)
	}
	// spec-violating upstream: arguments as a raw object must parse, not error
	c2, err := ParseStreamChunk([]byte(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"file","arguments":{"a":1}}}]},"finish_reason":null}]}`))
	if err != nil {
		t.Fatalf("object arguments must not error: %v", err)
	}
	if c2.ToolCalls[0].Function.Arguments != `{"a":1}` {
		t.Errorf("got %q", c2.ToolCalls[0].Function.Arguments)
	}
}

func TestEncodeChatRequestParallelToolCalls(t *testing.T) {
	f := false
	b, err := EncodeChatRequest(llm.ChatRequest{Model: "m", ParallelToolCalls: &f}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"parallel_tool_calls":false`) {
		t.Errorf("flag dropped: %s", b)
	}
	b2, _ := EncodeChatRequest(llm.ChatRequest{Model: "m"}, false)
	if strings.Contains(string(b2), "parallel_tool_calls") {
		t.Errorf("nil flag must be omitted: %s", b2)
	}
}
