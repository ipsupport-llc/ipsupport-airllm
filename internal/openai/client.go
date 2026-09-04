package openai

import (
	"encoding/json"
	"io"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/llm"
)

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// openaiOutMessage is the wire shape for one message sent to the real
// upstream. Content is `any` because it's a plain string for the common
// no-image case (byte-identical to today's output) or a []any of
// text/image_url parts when the message carries images. This is the
// ONLY place raw image data is ever read from Message.Images to build
// output — deliberately not a method on llm.Message itself, so no
// future generic json.Marshal on llm.Message (e.g. the capture
// pipeline's) can accidentally invoke it. See the design spec's
// "Deliberately NOT adding a matching MarshalJSON" section.
type openaiOutMessage struct {
	Role       string         `json:"role"`
	Content    any            `json:"content,omitempty"`
	Name       string         `json:"name,omitempty"`
	ToolCalls  []llm.ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type outTextPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type outImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

type outImagePart struct {
	Type     string      `json:"type"`
	ImageURL outImageURL `json:"image_url"`
}

func toOpenAIOutMessage(m llm.Message) openaiOutMessage {
	out := openaiOutMessage{Role: m.Role, Name: m.Name, ToolCalls: m.ToolCalls, ToolCallID: m.ToolCallID}
	if len(m.Images) == 0 {
		// IMPORTANT: only assign when non-empty. `Content any` with
		// `omitempty` only omits a truly-nil interface — assigning the
		// empty string "" (even though it IS the empty string) would
		// leave the interface non-nil, so `omitempty` would NOT omit it,
		// and a tool-calls-only assistant message (Content == "") would
		// regress from omitting "content" entirely (today's exact
		// behavior, since Message.Content is a plain string field there)
		// to emitting `"content":""`. Leaving out.Content as its zero
		// value (untyped nil) when Content == "" preserves today's
		// omitted-field behavior exactly.
		if m.Content != "" {
			out.Content = m.Content
		}
		return out
	}
	var parts []any
	if m.Content != "" {
		parts = append(parts, outTextPart{Type: "text", Text: m.Content})
	}
	for _, img := range m.Images {
		parts = append(parts, outImagePart{Type: "image_url", ImageURL: outImageURL{URL: img.URL, Detail: img.Detail}})
	}
	out.Content = parts
	return out
}

type upstreamChatRequest struct {
	Model             string             `json:"model"`
	Messages          []openaiOutMessage `json:"messages"`
	Tools             []llm.Tool         `json:"tools,omitempty"`
	ToolChoice        json.RawMessage    `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool              `json:"parallel_tool_calls,omitempty"`
	Temperature       *float64           `json:"temperature,omitempty"`
	MaxTokens         *int               `json:"max_tokens,omitempty"`
	Stream            bool               `json:"stream,omitempty"`
	StreamOptions     *streamOptions     `json:"stream_options,omitempty"`
}

// EncodeChatRequest renders the IR as an OpenAI chat-completions request body
// for an upstream call. When streaming, it asks for a final usage chunk.
func EncodeChatRequest(req llm.ChatRequest, stream bool) ([]byte, error) {
	outMsgs := make([]openaiOutMessage, len(req.Messages))
	for i, m := range req.Messages {
		outMsgs[i] = toOpenAIOutMessage(m)
	}
	u := upstreamChatRequest{
		Model:             req.Model,
		Messages:          outMsgs,
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
	b, err := json.Marshal(u)
	if err != nil || len(req.Extra) == 0 {
		return b, err
	}
	var merged map[string]json.RawMessage
	if err := json.Unmarshal(b, &merged); err != nil {
		return nil, err
	}
	for k, v := range req.Extra {
		merged[k] = v
	}
	return json.Marshal(merged)
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
