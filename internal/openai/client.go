package openai

import (
	"encoding/json"
	"io"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/llm"
)

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type upstreamChatRequest struct {
	Model             string          `json:"model"`
	Messages          []llm.Message   `json:"messages"`
	Tools             []llm.Tool      `json:"tools,omitempty"`
	ToolChoice        json.RawMessage `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool           `json:"parallel_tool_calls,omitempty"`
	Temperature       *float64        `json:"temperature,omitempty"`
	MaxTokens         *int            `json:"max_tokens,omitempty"`
	Stream            bool            `json:"stream,omitempty"`
	StreamOptions     *streamOptions  `json:"stream_options,omitempty"`
}

// EncodeChatRequest renders the IR as an OpenAI chat-completions request body
// for an upstream call. When streaming, it asks for a final usage chunk.
func EncodeChatRequest(req llm.ChatRequest, stream bool) ([]byte, error) {
	u := upstreamChatRequest{
		Model:             req.Model,
		Messages:          req.Messages,
		Tools:             req.Tools,
		ToolChoice:        req.ToolChoice,
		ParallelToolCalls: req.ParallelToolCalls,
		Temperature:       req.Temperature,
		MaxTokens:         req.MaxTokens,
		Stream:            stream,
	}
	if stream {
		u.StreamOptions = &streamOptions{IncludeUsage: true}
	}
	return json.Marshal(u)
}

type upstreamResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Created int64  `json:"created"`
	Choices []struct {
		Index        int         `json:"index"`
		Message      llm.Message `json:"message"`
		FinishReason string      `json:"finish_reason"`
	} `json:"choices"`
	Usage llm.Usage `json:"usage"`
}

// DecodeChatResponse parses an upstream OpenAI chat-completions response.
func DecodeChatResponse(r io.Reader) (llm.ChatResponse, error) {
	var w upstreamResponse
	if err := json.NewDecoder(r).Decode(&w); err != nil {
		return llm.ChatResponse{}, err
	}
	out := llm.ChatResponse{ID: w.ID, Model: w.Model, Created: w.Created, Usage: w.Usage}
	for _, c := range w.Choices {
		out.Choices = append(out.Choices, llm.Choice{
			Index:        c.Index,
			Message:      c.Message,
			FinishReason: c.FinishReason,
		})
	}
	return out, nil
}

type upstreamChunk struct {
	Choices []struct {
		Delta struct {
			Role      string              `json:"role"`
			Content   string              `json:"content"`
			ToolCalls []llm.ToolCallDelta `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *llm.Usage `json:"usage"`
}

// ParseStreamChunk parses one OpenAI SSE data payload into an IR chunk.
func ParseStreamChunk(data []byte) (llm.StreamChunk, error) {
	var w upstreamChunk
	if err := json.Unmarshal(data, &w); err != nil {
		return llm.StreamChunk{}, err
	}
	var c llm.StreamChunk
	if w.Usage != nil {
		c.Usage = w.Usage
	}
	if len(w.Choices) > 0 {
		d := w.Choices[0]
		c.Role = d.Delta.Role
		c.Content = d.Delta.Content
		c.ToolCalls = d.Delta.ToolCalls
		if d.FinishReason != nil {
			c.FinishReason = *d.FinishReason
		}
	}
	return c, nil
}
