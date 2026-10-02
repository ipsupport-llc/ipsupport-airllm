package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/llm"
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

// TestDecodeInterleavedToolResultsPreserveOrder proves that tool_result
// blocks interleaved with text within one user turn keep their original
// relative order (Protocol translation C2 fix) — the prior code grouped ALL
// text into one message placed before ALL tool_results, losing the actual
// sequence: tool_result(t1), text, tool_result(t2) must decode to exactly
// that order, not tool_result(t1), tool_result(t2), text.
func TestDecodeInterleavedToolResultsPreserveOrder(t *testing.T) {
	body := `{
		"model": "claude-x",
		"max_tokens": 50,
		"messages": [
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"t1","content":"result one"},
				{"type":"text","text":"also check this"},
				{"type":"tool_result","tool_use_id":"t2","content":"result two"}
			]}
		]
	}`
	req, err := DecodeMessagesRequest(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Messages) != 3 {
		t.Fatalf("want 3 IR messages, got %d: %+v", len(req.Messages), req.Messages)
	}
	if req.Messages[0].Role != "tool" || req.Messages[0].ToolCallID != "t1" || req.Messages[0].Content != "result one" {
		t.Errorf("message 0 wrong: %+v", req.Messages[0])
	}
	if req.Messages[1].Role != "user" || req.Messages[1].Content != "also check this" {
		t.Errorf("message 1 wrong: %+v", req.Messages[1])
	}
	if req.Messages[2].Role != "tool" || req.Messages[2].ToolCallID != "t2" || req.Messages[2].Content != "result two" {
		t.Errorf("message 2 wrong: %+v", req.Messages[2])
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

// TestMarshalResponseTextAndToolUse proves that assistant commentary text
// is NOT dropped when the same turn also carries a tool call (Protocol
// translation C1 fix) — e.g. a model emitting "Let me check that." followed
// by a tool_use. The text block must come first, matching how real
// Anthropic models order their own content blocks.
func TestMarshalResponseTextAndToolUse(t *testing.T) {
	resp := llm.ChatResponse{
		Model: "claude-x",
		Choices: []llm.Choice{{
			Message: llm.Message{
				Role:    "assistant",
				Content: "Let me check that.",
				ToolCalls: []llm.ToolCall{{
					ID: "tc1", Type: "function",
					Function: llm.FunctionCall{Name: "search", Arguments: `{"q":"x"}`},
				}},
			},
			FinishReason: "tool_calls",
		}},
	}
	b, err := MarshalMessagesResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	var out messageResponseWire
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(out.Content) != 2 {
		t.Fatalf("expected 2 content blocks (text + tool_use), got %d: %s", len(out.Content), b)
	}
	if out.Content[0].Type != "text" || out.Content[0].Text != "Let me check that." {
		t.Errorf("expected text block first, got %+v", out.Content[0])
	}
	if out.Content[1].Type != "tool_use" || out.Content[1].Name != "search" {
		t.Errorf("expected tool_use block second, got %+v", out.Content[1])
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

func TestDecodeImageBlockContent(t *testing.T) {
	body := `{
		"model": "claude-x",
		"max_tokens": 100,
		"messages": [{
			"role": "user",
			"content": [
				{"type": "text", "text": "what is this?"},
				{"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "AAAA"}}
			]
		}]
	}`
	req, err := DecodeMessagesRequest(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Messages) != 1 {
		t.Fatalf("want 1 message, got %d", len(req.Messages))
	}
	m := req.Messages[0]
	if m.Content != "what is this?" {
		t.Errorf("Content = %q", m.Content)
	}
	if len(m.Images) != 1 || m.Images[0].URL != "data:image/png;base64,AAAA" {
		t.Errorf("Images = %+v", m.Images)
	}
}

func TestDecodeImageOnlyMessageNotDropped(t *testing.T) {
	body := `{
		"model": "claude-x",
		"max_tokens": 100,
		"messages": [{
			"role": "user",
			"content": [{"type": "image", "source": {"type": "base64", "media_type": "image/jpeg", "data": "BBBB"}}]
		}]
	}`
	req, err := DecodeMessagesRequest(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Messages) != 1 {
		t.Fatalf("an image-only message must not be silently dropped — got %d messages", len(req.Messages))
	}
	if req.Messages[0].Content != "" {
		t.Errorf("Content = %q, want empty", req.Messages[0].Content)
	}
	if len(req.Messages[0].Images) != 1 || req.Messages[0].Images[0].URL != "data:image/jpeg;base64,BBBB" {
		t.Errorf("Images = %+v", req.Messages[0].Images)
	}
}

func TestDecodeURLSourceImageBlock(t *testing.T) {
	body := `{
		"model": "claude-x",
		"max_tokens": 100,
		"messages": [{
			"role": "user",
			"content": [{"type": "image", "source": {"type": "url", "url": "https://example.com/a.png"}}]
		}]
	}`
	req, err := DecodeMessagesRequest(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Messages) != 1 {
		t.Fatalf("url-source image-only message must not be dropped — got %d messages", len(req.Messages))
	}
	if len(req.Messages[0].Images) != 1 || req.Messages[0].Images[0].URL != "https://example.com/a.png" {
		t.Errorf("Images = %+v", req.Messages[0].Images)
	}
}

func TestDecodeUnsupportedImageSourceDoesNotPanic(t *testing.T) {
	body := `{
		"model": "claude-x",
		"max_tokens": 100,
		"messages": [{
			"role": "user",
			"content": [
				{"type": "text", "text": "hi"},
				{"type": "image", "source": {"type": "something-future"}}
			]
		}]
	}`
	req, err := DecodeMessagesRequest(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Messages) != 1 || req.Messages[0].Content != "hi" {
		t.Fatalf("expected the text part to still decode normally, got %+v", req.Messages)
	}
	if len(req.Messages[0].Images) != 0 {
		t.Errorf("an unsupported source type must not produce a bogus Image, got %+v", req.Messages[0].Images)
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
