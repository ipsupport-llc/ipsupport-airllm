package openai

import (
	"encoding/json"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/llm"
)

// StreamMeta is the stable identity shared by every chunk of one streamed
// response.
type StreamMeta struct {
	ID      string
	Model   string
	Created int64
}

type streamToolFunc struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type streamToolCall struct {
	Index    int            `json:"index"`
	ID       string         `json:"id,omitempty"`
	Type     string         `json:"type,omitempty"`
	Function streamToolFunc `json:"function"`
}

type streamDelta struct {
	Role      string           `json:"role,omitempty"`
	Content   string           `json:"content,omitempty"`
	ToolCalls []streamToolCall `json:"tool_calls,omitempty"`
}

type streamChoice struct {
	Index        int         `json:"index"`
	Delta        streamDelta `json:"delta"`
	FinishReason *string     `json:"finish_reason"`
}

type streamChunkWire struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []streamChoice `json:"choices"`
	Usage   *llm.Usage     `json:"usage,omitempty"`
}

// MarshalStreamChunk renders one llm.StreamChunk as OpenAI
// chat.completion.chunk JSON. A usage-only chunk yields an empty choices
// array with the usage object, matching stream_options.include_usage.
func MarshalStreamChunk(meta StreamMeta, c llm.StreamChunk) ([]byte, error) {
	wire := streamChunkWire{
		ID:      meta.ID,
		Object:  "chat.completion.chunk",
		Created: meta.Created,
		Model:   meta.Model,
	}

	if c.Usage != nil {
		wire.Choices = []streamChoice{}
		wire.Usage = c.Usage
		return json.Marshal(wire)
	}

	choice := streamChoice{Index: 0}
	choice.Delta.Role = c.Role
	choice.Delta.Content = c.Content
	for i, tc := range c.ToolCalls {
		choice.Delta.ToolCalls = append(choice.Delta.ToolCalls, streamToolCall{
			Index:    i,
			ID:       tc.ID,
			Type:     tc.Type,
			Function: streamToolFunc{Name: tc.Function.Name, Arguments: tc.Function.Arguments},
		})
	}
	if c.FinishReason != "" {
		fr := c.FinishReason
		choice.FinishReason = &fr
	}
	wire.Choices = []streamChoice{choice}
	return json.Marshal(wire)
}
