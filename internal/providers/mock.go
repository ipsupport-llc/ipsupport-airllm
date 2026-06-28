package providers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/llm"
)

// Mock is an in-process provider that simulates an OpenAI-shaped upstream:
// it echoes the last user turn, reports plausible token usage, and supports
// both non-streaming and streaming responses. It lets the whole pipeline run
// locally without real provider credentials or spend.
//
// Tool calls: to keep the mock usable by real coding agents (which always
// send tools), it returns normal content by default and emits a tool call
// only when tools are present AND the last user message contains the trigger
// substring "tooltest".
type Mock struct {
	name string
}

// NewMock returns a mock provider with the given name.
func NewMock(name string) *Mock { return &Mock{name: name} }

func (m *Mock) Name() string     { return m.name }
func (m *Mock) Kind() string     { return "mock" }
func (m *Mock) Protocol() string { return "openai" }

// Chat returns a deterministic mock completion for req. An upstream model
// containing "fail" yields a retryable error, used to exercise routing
// fallback.
func (m *Mock) Chat(_ context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
	if strings.Contains(req.Model, "fail") {
		return llm.ChatResponse{}, &Error{Status: 503, Retryable: true, Message: "mock upstream failure for model " + req.Model}
	}
	if strings.Contains(req.Model, "slow") {
		time.Sleep(300 * time.Millisecond) // hold a concurrency slot, for testing saturation
	}
	prompt := approxTokens(joinMessages(req.Messages))
	base := llm.ChatResponse{
		ID:      "chatcmpl-mock-" + randID(),
		Model:   req.Model,
		Created: time.Now().Unix(),
	}

	if m.wantsToolCall(req) {
		tc := m.toolCall(req)
		base.Choices = []llm.Choice{{
			Index:        0,
			Message:      llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{tc}},
			FinishReason: "tool_calls",
		}}
		completion := approxTokens(tc.Function.Name + tc.Function.Arguments)
		base.Usage = llm.Usage{PromptTokens: prompt, CompletionTokens: completion, TotalTokens: prompt + completion}
		return base, nil
	}

	content := m.content(req)
	base.Choices = []llm.Choice{{
		Index:        0,
		Message:      llm.Message{Role: "assistant", Content: content},
		FinishReason: "stop",
	}}
	completion := approxTokens(content)
	base.Usage = llm.Usage{PromptTokens: prompt, CompletionTokens: completion, TotalTokens: prompt + completion}
	return base, nil
}

// ChatStream emits the same logical response as Chat, chunk by chunk. It
// fails (before yielding anything) for "fail" models so the router can fall
// back to the next target.
func (m *Mock) ChatStream(_ context.Context, req llm.ChatRequest, yield func(llm.StreamChunk) error) error {
	if strings.Contains(req.Model, "fail") {
		return &Error{Status: 503, Retryable: true, Message: "mock upstream failure for model " + req.Model}
	}
	if err := yield(llm.StreamChunk{Role: "assistant"}); err != nil {
		return err
	}
	prompt := approxTokens(joinMessages(req.Messages))

	if m.wantsToolCall(req) {
		tc := m.toolCall(req)
		if err := yield(llm.StreamChunk{ToolCalls: []llm.ToolCall{tc}}); err != nil {
			return err
		}
		if err := yield(llm.StreamChunk{FinishReason: "tool_calls"}); err != nil {
			return err
		}
		completion := approxTokens(tc.Function.Name + tc.Function.Arguments)
		return yield(llm.StreamChunk{Usage: &llm.Usage{PromptTokens: prompt, CompletionTokens: completion, TotalTokens: prompt + completion}})
	}

	content := m.content(req)
	for _, piece := range splitForStream(content) {
		if err := yield(llm.StreamChunk{Content: piece}); err != nil {
			return err
		}
	}
	if err := yield(llm.StreamChunk{FinishReason: "stop"}); err != nil {
		return err
	}
	completion := approxTokens(content)
	return yield(llm.StreamChunk{Usage: &llm.Usage{PromptTokens: prompt, CompletionTokens: completion, TotalTokens: prompt + completion}})
}

func (m *Mock) content(req llm.ChatRequest) string {
	return fmt.Sprintf(
		"Mock response from provider %q (model %q). You said: %s",
		m.name, req.Model, lastUserText(req.Messages))
}

func (m *Mock) wantsToolCall(req llm.ChatRequest) bool {
	return len(req.Tools) > 0 &&
		strings.Contains(strings.ToLower(lastUserText(req.Messages)), "tooltest")
}

func (m *Mock) toolCall(req llm.ChatRequest) llm.ToolCall {
	return llm.ToolCall{
		ID:   "call_mock_" + randID(),
		Type: "function",
		Function: llm.FunctionCall{
			Name:      req.Tools[0].Function.Name,
			Arguments: "{}",
		},
	}
}

func lastUserText(msgs []llm.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			return msgs[i].Content
		}
	}
	return ""
}

func joinMessages(msgs []llm.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		b.WriteString(m.Content)
		b.WriteByte('\n')
	}
	return b.String()
}

// splitForStream breaks text into word-sized pieces (trailing space kept)
// to simulate token-by-token streaming.
func splitForStream(s string) []string {
	words := strings.Fields(s)
	out := make([]string, 0, len(words))
	for i, w := range words {
		if i < len(words)-1 {
			w += " "
		}
		out = append(out, w)
	}
	return out
}

// approxTokens is a crude rune/4 token estimate, used only by the mock.
func approxTokens(s string) int {
	n := len([]rune(s)) / 4
	if n < 1 {
		n = 1
	}
	return n
}

func randID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
