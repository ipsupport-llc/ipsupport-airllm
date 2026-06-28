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

// FunctionCall carries the called function name and JSON-string arguments.
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
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
	Model       string
	Messages    []Message
	Tools       []Tool
	ToolChoice  json.RawMessage
	Temperature *float64
	MaxTokens   *int
	Stream      bool
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
	Role         string     // set on the first chunk only
	Content      string     // incremental text
	ToolCalls    []ToolCall // incremental tool calls (the mock emits whole)
	FinishReason string     // "stop" | "tool_calls" on the final delta chunk
	Usage        *Usage     // set on the terminal usage chunk only
}
