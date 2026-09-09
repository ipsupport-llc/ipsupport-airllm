package providers

import (
	"context"
	"strings"
	"testing"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/audio"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/llm"
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

func TestMockCtxFailFallbackWorthy(t *testing.T) {
	m := NewMock("mock")

	ctxFailReq := llm.ChatRequest{Model: "mock-ctxfail", Messages: []llm.Message{{Role: "user", Content: "x"}}}
	_, err := m.Chat(context.Background(), ctxFailReq)
	if err == nil {
		t.Fatal("ctxfail model must return an error")
	}
	if IsRetryable(err) {
		t.Error("ctxfail error must NOT be Retryable (it's a target-specific failure, not transient)")
	}
	if !IsFallbackWorthy(err) {
		t.Error("ctxfail error must be fallback-worthy via its Code")
	}

	err = m.ChatStream(context.Background(), ctxFailReq, func(llm.StreamChunk) error {
		t.Fatal("ctxfail model must not yield any chunk")
		return nil
	})
	if !IsFallbackWorthy(err) {
		t.Error("ChatStream ctxfail error must be fallback-worthy via its Code")
	}
}

func TestMockNovisionFallbackWorthy(t *testing.T) {
	m := NewMock("mock")

	novisionReq := llm.ChatRequest{Model: "mock-novision", Messages: []llm.Message{{Role: "user", Content: "x"}}}
	_, err := m.Chat(context.Background(), novisionReq)
	if err == nil {
		t.Fatal("novision model must return an error")
	}
	pe, ok := err.(*Error)
	if !ok {
		t.Fatalf("want *Error, got %T", err)
	}
	if pe.Code != ErrCodeMultimodalNotSupported {
		t.Errorf("Code = %q, want %q", pe.Code, ErrCodeMultimodalNotSupported)
	}
	if IsRetryable(err) {
		t.Error("novision error must NOT be Retryable (it's a target-specific failure, not transient)")
	}
	if !IsFallbackWorthy(err) {
		t.Error("novision error must be fallback-worthy via its Code")
	}

	err = m.ChatStream(context.Background(), novisionReq, func(llm.StreamChunk) error {
		t.Fatal("novision model must not yield any chunk")
		return nil
	})
	if !IsFallbackWorthy(err) {
		t.Error("ChatStream novision error must be fallback-worthy via its Code")
	}
}

func TestMockNoreasoningFallbackWorthy(t *testing.T) {
	m := NewMock("mock")

	noreasoningReq := llm.ChatRequest{Model: "mock-noreasoning", Messages: []llm.Message{{Role: "user", Content: "x"}}}
	_, err := m.Chat(context.Background(), noreasoningReq)
	if err == nil {
		t.Fatal("noreasoning model must return an error")
	}
	pe, ok := err.(*Error)
	if !ok {
		t.Fatalf("want *Error, got %T", err)
	}
	if pe.Code != ErrCodeReasoningEffortUnsupported {
		t.Errorf("Code = %q, want %q", pe.Code, ErrCodeReasoningEffortUnsupported)
	}
	if IsRetryable(err) {
		t.Error("noreasoning error must NOT be Retryable (it's a target-specific failure, not transient)")
	}
	if !IsFallbackWorthy(err) {
		t.Error("noreasoning error must be fallback-worthy via its Code")
	}

	err = m.ChatStream(context.Background(), noreasoningReq, func(llm.StreamChunk) error {
		t.Fatal("noreasoning model must not yield any chunk")
		return nil
	})
	if !IsFallbackWorthy(err) {
		t.Error("ChatStream noreasoning error must be fallback-worthy via its Code")
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

func TestMockTranscribe(t *testing.T) {
	m := NewMock("mock")
	resp, err := m.Transcribe(context.Background(), audio.TranscriptionRequest{
		Model: "mock-whisper", Audio: []byte("fake-audio-bytes"), Filename: "clip.wav",
	})
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if resp.Text == "" {
		t.Error("want non-empty transcript")
	}
	if resp.DurationSeconds <= 0 {
		t.Errorf("DurationSeconds = %v, want > 0", resp.DurationSeconds)
	}
}

func TestMockSynthesize(t *testing.T) {
	m := NewMock("mock")
	resp, err := m.Synthesize(context.Background(), audio.SpeechRequest{
		Model: "mock-tts", Input: "hello world", Voice: "default",
	})
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if len(resp.Audio) == 0 {
		t.Error("want non-empty audio bytes")
	}
	if resp.ContentType == "" {
		t.Error("want a non-empty ContentType")
	}
}

func TestMockChatEchoesImageCount(t *testing.T) {
	m := NewMock("mock")
	req := llm.ChatRequest{
		Model: "mock-gpt",
		Messages: []llm.Message{{
			Role: "user", Content: "what is this",
			Images: []llm.Image{{URL: "data:image/png;base64,AAAA"}, {URL: "data:image/png;base64,BBBB"}},
		}},
	}
	resp, err := m.Chat(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	content := resp.Choices[0].Message.Content
	if !strings.Contains(content, "2 image(s) attached") {
		t.Errorf("expected image count echoed, got %q", content)
	}
}

func TestMockChatNoImagesUnaffected(t *testing.T) {
	m := NewMock("mock")
	resp, err := m.Chat(context.Background(), userReq("hello world"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(resp.Choices[0].Message.Content, "image") {
		t.Errorf("a request with no images must not mention images: %q", resp.Choices[0].Message.Content)
	}
}
