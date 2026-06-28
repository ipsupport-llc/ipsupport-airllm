package providers

import (
	"context"
	"strings"
	"testing"

	"github.com/rromenskyi/ipsupport-airllm/internal/llm"
)

func userReq(text string, tools ...llm.Tool) llm.ChatRequest {
	return llm.ChatRequest{
		Model:    "mock-gpt",
		Messages: []llm.Message{{Role: "user", Content: text}},
		Tools:    tools,
	}
}

func TestMockChatContent(t *testing.T) {
	m := NewMock("mock")
	resp, err := m.Chat(context.Background(), userReq("hello there"))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("want 1 choice, got %d", len(resp.Choices))
	}
	c := resp.Choices[0]
	if c.FinishReason != "stop" {
		t.Errorf("finish_reason = %q, want stop", c.FinishReason)
	}
	if !strings.Contains(c.Message.Content, "hello there") {
		t.Errorf("content does not echo user: %q", c.Message.Content)
	}
	if resp.Usage.TotalTokens != resp.Usage.PromptTokens+resp.Usage.CompletionTokens {
		t.Errorf("usage total mismatch: %+v", resp.Usage)
	}
}

func TestMockToolCallTrigger(t *testing.T) {
	m := NewMock("mock")
	tool := llm.Tool{Type: "function", Function: llm.FunctionDef{Name: "search"}}

	// No trigger word -> normal content even with tools present.
	resp, _ := m.Chat(context.Background(), userReq("just chat", tool))
	if resp.Choices[0].FinishReason != "stop" {
		t.Errorf("expected content response without trigger, got %q", resp.Choices[0].FinishReason)
	}

	// Trigger word -> tool call.
	resp, _ = m.Chat(context.Background(), userReq("please tooltest now", tool))
	c := resp.Choices[0]
	if c.FinishReason != "tool_calls" {
		t.Fatalf("expected tool_calls finish, got %q", c.FinishReason)
	}
	if len(c.Message.ToolCalls) != 1 || c.Message.ToolCalls[0].Function.Name != "search" {
		t.Errorf("expected one tool call to search, got %+v", c.Message.ToolCalls)
	}
}

func TestMockChatStreamSequence(t *testing.T) {
	m := NewMock("mock")
	var chunks []llm.StreamChunk
	err := m.ChatStream(context.Background(), userReq("stream this"), func(c llm.StreamChunk) error {
		chunks = append(chunks, c)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) < 3 {
		t.Fatalf("want several chunks, got %d", len(chunks))
	}
	if chunks[0].Role != "assistant" {
		t.Errorf("first chunk should set role, got %+v", chunks[0])
	}
	last := chunks[len(chunks)-1]
	if last.Usage == nil {
		t.Errorf("last chunk should carry usage, got %+v", last)
	}

	var content strings.Builder
	sawFinish := false
	for _, c := range chunks {
		content.WriteString(c.Content)
		if c.FinishReason == "stop" {
			sawFinish = true
		}
	}
	if !sawFinish {
		t.Error("stream never sent a stop finish_reason")
	}
	if !strings.Contains(content.String(), "stream this") {
		t.Errorf("streamed content does not echo user: %q", content.String())
	}
}

func TestMockFailRetryable(t *testing.T) {
	m := NewMock("mock")
	_, err := m.Chat(context.Background(), userReq("x"))
	if err != nil {
		t.Fatalf("non-fail model should succeed: %v", err)
	}

	failReq := llm.ChatRequest{Model: "mock-fail", Messages: []llm.Message{{Role: "user", Content: "x"}}}
	_, err = m.Chat(context.Background(), failReq)
	if !IsRetryable(err) {
		t.Errorf("Chat fail model: expected retryable error, got %v", err)
	}

	err = m.ChatStream(context.Background(), failReq, func(llm.StreamChunk) error {
		t.Fatal("fail model must not yield any chunk")
		return nil
	})
	if !IsRetryable(err) {
		t.Errorf("ChatStream fail model: expected retryable error, got %v", err)
	}
}

func TestMockChatStreamYieldError(t *testing.T) {
	m := NewMock("mock")
	sentinel := context.Canceled
	calls := 0
	err := m.ChatStream(context.Background(), userReq("stop early"), func(c llm.StreamChunk) error {
		calls++
		return sentinel
	})
	if err != sentinel {
		t.Errorf("ChatStream should return yield error, got %v", err)
	}
	if calls != 1 {
		t.Errorf("ChatStream should stop after first yield error, made %d calls", calls)
	}
}
