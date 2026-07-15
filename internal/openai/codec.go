// Package openai encodes/decodes the OpenAI chat-completions wire format
// to and from the provider-neutral llm IR.
package openai

import (
	"encoding/json"
	"errors"
	"io"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/llm"
)

type chatRequestWire struct {
	Model             string          `json:"model"`
	Messages          []llm.Message   `json:"messages"`
	Tools             []llm.Tool      `json:"tools,omitempty"`
	ToolChoice        json.RawMessage `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool           `json:"parallel_tool_calls,omitempty"`
	Temperature       *float64        `json:"temperature,omitempty"`
	MaxTokens         *int            `json:"max_tokens,omitempty"`
	Stream            bool            `json:"stream,omitempty"`
}

// DecodeChatRequest parses an OpenAI chat-completions request body.
func DecodeChatRequest(r io.Reader) (llm.ChatRequest, error) {
	var w chatRequestWire
	dec := json.NewDecoder(r)
	if err := dec.Decode(&w); err != nil {
		return llm.ChatRequest{}, err
	}
	if w.Model == "" {
		return llm.ChatRequest{}, errors.New("model is required")
	}
	if len(w.Messages) == 0 {
		return llm.ChatRequest{}, errors.New("messages is required")
	}
	return llm.ChatRequest{
		Model:             w.Model,
		Messages:          w.Messages,
		Tools:             w.Tools,
		ToolChoice:        w.ToolChoice,
		ParallelToolCalls: w.ParallelToolCalls,
		Temperature:       w.Temperature,
		MaxTokens:         w.MaxTokens,
		Stream:            w.Stream,
	}, nil
}

type choiceWire struct {
	Index        int         `json:"index"`
	Message      llm.Message `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

type chatResponseWire struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []choiceWire `json:"choices"`
	Usage   llm.Usage    `json:"usage"`
}

// MarshalChatResponse renders an llm.ChatResponse as OpenAI JSON bytes.
func MarshalChatResponse(resp llm.ChatResponse) ([]byte, error) {
	out := chatResponseWire{
		ID:      resp.ID,
		Object:  "chat.completion",
		Created: resp.Created,
		Model:   resp.Model,
		Usage:   resp.Usage,
	}
	for _, c := range resp.Choices {
		out.Choices = append(out.Choices, choiceWire{
			Index:        c.Index,
			Message:      c.Message,
			FinishReason: c.FinishReason,
		})
	}
	return json.Marshal(out)
}
