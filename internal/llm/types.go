// Package llm holds the provider-neutral intermediate representation for
// chat requests and responses. Ingress codecs decode client formats into
// these types; providers operate on them; egress codecs encode them back.
// The IR is OpenAI-shaped because most clients are; Anthropic maps onto it.
package llm

import "encoding/json"

// Message is one chat message.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	Name       string     `json:"name,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// ToolCall is a model-requested function call.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"` // always "function" today
	Function FunctionCall `json:"function"`
}

// ToolCallDelta is one streamed increment of a tool call. Index identifies
// the call within the message (OpenAI accumulation semantics): fragments
// sharing an Index belong to one call and their Arguments concatenate.
type ToolCallDelta struct {
	Index    int          `json:"index"`
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"`
	Function FunctionCall `json:"function"`
}

// FunctionCall carries the called function name and JSON-string arguments.
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// UnmarshalJSON accepts arguments as the spec's JSON-encoded string, but
// also tolerates upstreams that send a raw object or array (kept as its
// JSON text) or null/absent (empty string).
func (f *FunctionCall) UnmarshalJSON(b []byte) error {
	var w struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	f.Name = w.Name
	args := string(w.Arguments)
	if len(w.Arguments) == 0 || args == "null" {
		f.Arguments = ""
		return nil
	}
	var s string
	if err := json.Unmarshal(w.Arguments, &s); err == nil {
		f.Arguments = s
		return nil
	}
	f.Arguments = args
	return nil
}

// Tool is a function the model may call.
type Tool struct {
	Type     string      `json:"type"`
	Function FunctionDef `json:"function"`
}

// FunctionDef describes a callable function.
type FunctionDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// ChatRequest is a provider-neutral chat completion request.
type ChatRequest struct {
	Model             string
	Messages          []Message
	Tools             []Tool
	ToolChoice        json.RawMessage
	ParallelToolCalls *bool
	Temperature       *float64
	MaxTokens         *int
	Stream            bool

	// Extra carries unmapped OpenAI request fields verbatim (OpenAI ingress →
	// OpenAI-compatible upstream). Nil when the request had none.
	Extra map[string]json.RawMessage
}

// Usage is token accounting for one response.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Choice is one completion alternative.
type Choice struct {
	Index        int
	Message      Message
	FinishReason string
}

// ChatResponse is a provider-neutral chat completion response.
type ChatResponse struct {
	ID      string
	Model   string
	Created int64
	Choices []Choice
	Usage   Usage
}

// StreamChunk is one incremental piece of a streamed completion. A provider
// emits a sequence: an optional role chunk, content and/or tool-call deltas,
// a finish-reason chunk, and finally a usage chunk (Usage set, all else
// zero). Egress codecs translate these into the client's stream format.
type StreamChunk struct {
	Role         string          // set on the first chunk only
	Content      string          // incremental text
	ToolCalls    []ToolCallDelta // incremental tool calls; Index per OpenAI accumulation semantics
	FinishReason string          // "stop" | "tool_calls" on the final delta chunk
	Usage        *Usage          // set on the terminal usage chunk only
}
