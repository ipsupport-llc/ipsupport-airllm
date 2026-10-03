package providers

import (
	"encoding/json"
	"errors"
	"maps"
	"strings"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/llm"
)

// reasoningFields are the request fields an OpenAI-shaped client steers a
// model's thinking with. A thinking-off request forwards none of the client's.
var reasoningFields = []string{"reasoning_effort", "reasoning", "think"}

// thinkingOffSettings is each provider kind's lowest-thinking request
// setting. Every value was confirmed against the live upstream: Ollama turns
// thinking off with "none"; Vertex (Gemini 3) and xAI reject "none" with a
// 400 and go no lower than "minimal"; OpenRouter takes its own reasoning
// object. A kind missing here only has the client's fields dropped.
var thinkingOffSettings = map[string]map[string]json.RawMessage{
	"ollama":     {"reasoning_effort": json.RawMessage(`"none"`)},
	"vertex":     {"reasoning_effort": json.RawMessage(`"minimal"`)},
	"openrouter": {"reasoning": json.RawMessage(`{"effort":"none"}`)},
	"xai":        {"reasoning_effort": json.RawMessage(`"minimal"`)},
}

// withThinkingOff returns in with kind's lowest-thinking setting in place of
// the client's reasoning fields. A request without ThinkingOff is returned
// as it is.
func withThinkingOff(kind string, in llm.ChatRequest) llm.ChatRequest {
	if !in.ThinkingOff {
		return in
	}
	extra := maps.Clone(in.Extra)
	for _, f := range reasoningFields {
		delete(extra, f)
	}
	if set := thinkingOffSettings[kind]; len(set) > 0 {
		if extra == nil {
			extra = map[string]json.RawMessage{}
		}
		maps.Copy(extra, set)
	}
	if len(extra) == 0 {
		extra = nil
	}
	out := in
	out.Extra = extra
	return out
}

// thinkingRejection turns an upstream's 400 for the reasoning setting the
// gateway chose into a fallback-worthy error: a tier that cannot take its own
// configured setting is no reason to fail the request when another tier can
// serve it. Requests without ThinkingOff keep the error as it is — then the
// setting was the client's.
func thinkingRejection(in llm.ChatRequest, err error) error {
	var pe *Error
	if !in.ThinkingOff || !errors.As(err, &pe) || pe.Status != 400 || pe.Code != "" {
		return err
	}
	if !strings.Contains(strings.ToLower(pe.Message), "reasoning") {
		return err
	}
	cp := *pe
	cp.Code = ErrCodeReasoningEffortUnsupported
	return &cp
}
