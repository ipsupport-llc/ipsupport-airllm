package anthropic

import (
	"strings"
	"testing"

	"github.com/rromenskyi/ipsupport-airllm/internal/llm"
)

func TestDecodeStringContentWithSystemAndTools(t *testing.T) {
	body := `{
		"model": "claude-x",
		"max_tokens": 100,
		"system": "be brief",
		"tools": [{"name":"search","description":"d","input_schema":{"type":"object"}}],
		"messages": [{"role":"user","content":"hello"}]
	}`
	req, err := DecodeMessagesRequest(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if req.Model != "claude-x" {
		t.Errorf("model = %q", req.Model)
	}
	if req.MaxTokens == nil || *req.MaxTokens != 100 {
		t.Errorf("max_tokens not mapped: %v", req.MaxTokens)
	}
	if len(req.Messages) != 2 || req.Messages[0].Role != "system" || req.Messages[1].Content != "hello" {
		t.Errorf("messages mapping wrong: %+v", req.Messages)
	}
	if len(req.Tools) != 1 || req.Tools[0].Function.Name != "search" {
		t.Errorf("tools mapping wrong: %+v", req.Tools)
	}
}

func TestDecodeBlockContentAndToolResult(t *testing.T) {
	body := `{
		"model": "claude-x",
		"max_tokens": 50,
		"messages": [
			{"role":"assistant","content":[{"type":"text","text":"hi "},{"type":"tool_use","id":"t1","name":"search","input":{"q":"x"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"result text"}]}
		]
	}`
	req, err := DecodeMessagesRequest(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	// assistant message (text+tool_use), then tool result message.
	if len(req.Messages) != 2 {
		t.Fatalf("want 2 IR messages, got %d: %+v", len(req.Messages), req.Messages)
	}
	a := req.Messages[0]
	if a.Role != "assistant" || a.Content != "hi " || len(a.ToolCalls) != 1 || a.ToolCalls[0].ID != "t1" {
		t.Errorf("assistant mapping wrong: %+v", a)
	}
	tr := req.Messages[1]
	if tr.Role != "tool" || tr.ToolCallID != "t1" || tr.Content != "result text" {
		t.Errorf("tool_result mapping wrong: %+v", tr)
	}
}

func TestMarshalResponseText(t *testing.T) {
	resp := llm.ChatResponse{
		Model: "claude-x",
		Choices: []llm.Choice{{
			Message:      llm.Message{Role: "assistant", Content: "hello world"},
			FinishReason: "stop",
		}},
		Usage: llm.Usage{PromptTokens: 3, CompletionTokens: 2},
	}
	b, err := MarshalMessagesResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{
		`"type":"message"`, `"role":"assistant"`,
		`"type":"text"`, `"text":"hello world"`,
		`"stop_reason":"end_turn"`,
		`"input_tokens":3`, `"output_tokens":2`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("response missing %s in %s", want, s)
		}
	}
}

func TestMarshalResponseToolUse(t *testing.T) {
	resp := llm.ChatResponse{
		Model: "claude-x",
		Choices: []llm.Choice{{
			Message: llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{
				ID: "tc1", Type: "function",
				Function: llm.FunctionCall{Name: "search", Arguments: `{"q":"x"}`},
			}}},
			FinishReason: "tool_calls",
		}},
	}
	b, err := MarshalMessagesResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{`"type":"tool_use"`, `"name":"search"`, `"input":{"q":"x"}`, `"stop_reason":"tool_use"`} {
		if !strings.Contains(s, want) {
			t.Errorf("tool-use response missing %s in %s", want, s)
		}
	}
}

func TestStopReason(t *testing.T) {
	cases := map[string]string{"stop": "end_turn", "tool_calls": "tool_use", "length": "max_tokens", "": "end_turn"}
	for in, want := range cases {
		if got := StopReason(in); got != want {
			t.Errorf("StopReason(%q) = %q, want %q", in, got, want)
		}
	}
}

type bufFlusher struct{ b strings.Builder }

func (f *bufFlusher) Write(p []byte) (int, error) { return f.b.Write(p) }

func TestStreamWriterEvents(t *testing.T) {
	f := &bufFlusher{}
	sw := NewStreamWriter(f, func() {}, "msg_1", "claude-x", 5)
	chunks := []llm.StreamChunk{
		{Role: "assistant"},
		{Content: "Hello "},
		{Content: "world"},
		{FinishReason: "stop"},
		{Usage: &llm.Usage{PromptTokens: 5, CompletionTokens: 2}},
	}
	for _, c := range chunks {
		if err := sw.Chunk(c); err != nil {
			t.Fatal(err)
		}
	}
	out := f.b.String()
	for _, want := range []string{
		"event: message_start", "event: content_block_start",
		"event: content_block_delta", "event: content_block_stop",
		"event: message_delta", "event: message_stop",
		`"input_tokens":5`, `"output_tokens":2`, `"stop_reason":"end_turn"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stream missing %q\n---\n%s", want, out)
		}
	}
}
