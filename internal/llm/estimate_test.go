package llm

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEstimateTokens(t *testing.T) {
	for s, want := range map[string]int{"": 0, "a": 1, "abcd": 1, "abcdefgh": 2, "привет мир!!": 3} {
		if got := EstimateTokens(s); got != want {
			t.Errorf("EstimateTokens(%q) = %d, want %d", s, got, want)
		}
	}
}

func TestEstimatePromptTokensCountsToolsAndToolCalls(t *testing.T) {
	base := ChatRequest{Messages: []Message{{Role: "user", Content: strings.Repeat("a", 40)}}}
	if got := EstimatePromptTokens(base); got != 10 {
		t.Fatalf("plain prompt = %d, want 10", got)
	}
	full := base
	full.Messages = append(full.Messages, Message{Role: "assistant", ToolCalls: []ToolCall{{Function: FunctionCall{Name: "abcd", Arguments: strings.Repeat("b", 36)}}}})
	full.Tools = []Tool{{Function: FunctionDef{Name: "abcd", Description: strings.Repeat("c", 16), Parameters: json.RawMessage(`{"type":"object"}`)}}}
	if got, want := EstimatePromptTokens(full), (40+4+36+4+16+17)/4; got != want {
		t.Errorf("prompt with tools = %d, want %d", got, want)
	}
	if got := EstimatePromptTokens(ChatRequest{}); got != 1 {
		t.Errorf("empty prompt = %d, want at least 1", got)
	}
}
