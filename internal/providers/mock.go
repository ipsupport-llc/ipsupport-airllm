package providers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/rromenskyi/ipsupport-airouter/internal/llm"
)

// Mock is an in-process provider that simulates an OpenAI-shaped upstream:
// it echoes the last user turn, reports plausible token usage, and (when
// tools are offered) can emit a tool call. It lets the whole pipeline run
// locally without real provider credentials or spend.
type Mock struct {
	name string
}

// NewMock returns a mock provider with the given name.
func NewMock(name string) *Mock { return &Mock{name: name} }

func (m *Mock) Name() string     { return m.name }
func (m *Mock) Kind() string     { return "mock" }
func (m *Mock) Protocol() string { return "openai" }

// Chat returns a deterministic mock completion for req.
func (m *Mock) Chat(_ context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
	content := fmt.Sprintf(
		"Mock response from provider %q (model %q). You said: %s",
		m.name, req.Model, lastUserText(req.Messages))

	prompt := approxTokens(joinMessages(req.Messages))
	completion := approxTokens(content)

	return llm.ChatResponse{
		ID:      "chatcmpl-mock-" + randID(),
		Model:   req.Model,
		Created: time.Now().Unix(),
		Choices: []llm.Choice{{
			Index:        0,
			Message:      llm.Message{Role: "assistant", Content: content},
			FinishReason: "stop",
		}},
		Usage: llm.Usage{
			PromptTokens:     prompt,
			CompletionTokens: completion,
			TotalTokens:      prompt + completion,
		},
	}, nil
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
