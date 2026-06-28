// Package anthropic encodes/decodes the Anthropic Messages API wire format
// to and from the provider-neutral llm IR. The IR is OpenAI-shaped, so this
// package performs the cross-protocol mapping for the Anthropic ingress.
package anthropic

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/rromenskyi/ipsupport-airllm/internal/llm"
)

type messagesRequestWire struct {
	Model       string          `json:"model"`
	System      json.RawMessage `json:"system,omitempty"`
	Messages    []messageWire   `json:"messages"`
	Tools       []toolWire      `json:"tools,omitempty"`
	ToolChoice  json.RawMessage `json:"tool_choice,omitempty"`
	MaxTokens   int             `json:"max_tokens"`
	Temperature *float64        `json:"temperature,omitempty"`
	Stream      bool            `json:"stream,omitempty"`
}

type messageWire struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type toolWire struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

type contentBlockWire struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// tool_result
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
}

// DecodeMessagesRequest parses an Anthropic Messages request into the IR.
func DecodeMessagesRequest(r io.Reader) (llm.ChatRequest, error) {
	var w messagesRequestWire
	if err := json.NewDecoder(r).Decode(&w); err != nil {
		return llm.ChatRequest{}, err
	}
	if w.Model == "" {
		return llm.ChatRequest{}, errors.New("model is required")
	}
	if len(w.Messages) == 0 {
		return llm.ChatRequest{}, errors.New("messages is required")
	}

	var msgs []llm.Message
	if sys := blocksText(w.System); sys != "" {
		msgs = append(msgs, llm.Message{Role: "system", Content: sys})
	}
	for _, mw := range w.Messages {
		msgs = append(msgs, convertMessage(mw)...)
	}

	var tools []llm.Tool
	for _, tw := range w.Tools {
		tools = append(tools, llm.Tool{
			Type: "function",
			Function: llm.FunctionDef{
				Name:        tw.Name,
				Description: tw.Description,
				Parameters:  tw.InputSchema,
			},
		})
	}

	req := llm.ChatRequest{
		Model:       w.Model,
		Messages:    msgs,
		Tools:       tools,
		ToolChoice:  w.ToolChoice,
		Temperature: w.Temperature,
		Stream:      w.Stream,
	}
	if w.MaxTokens > 0 {
		mt := w.MaxTokens
		req.MaxTokens = &mt
	}
	return req, nil
}

// convertMessage maps one Anthropic message to one or more IR messages. A
// message bearing tool_result blocks splits those into IR "tool" messages.
func convertMessage(mw messageWire) []llm.Message {
	var s string
	if json.Unmarshal(mw.Content, &s) == nil {
		return []llm.Message{{Role: mw.Role, Content: s}}
	}

	var blocks []contentBlockWire
	if json.Unmarshal(mw.Content, &blocks) != nil {
		return []llm.Message{{Role: mw.Role}}
	}

	base := llm.Message{Role: mw.Role}
	var texts []string
	var toolResults []llm.Message
	for _, blk := range blocks {
		switch blk.Type {
		case "text":
			texts = append(texts, blk.Text)
		case "tool_use":
			args := string(blk.Input)
			if args == "" {
				args = "{}"
			}
			base.ToolCalls = append(base.ToolCalls, llm.ToolCall{
				ID:       blk.ID,
				Type:     "function",
				Function: llm.FunctionCall{Name: blk.Name, Arguments: args},
			})
		case "tool_result":
			toolResults = append(toolResults, llm.Message{
				Role:       "tool",
				ToolCallID: blk.ToolUseID,
				Content:    blocksText(blk.Content),
			})
		}
	}
	base.Content = strings.Join(texts, "")

	var out []llm.Message
	if base.Content != "" || len(base.ToolCalls) > 0 {
		out = append(out, base)
	}
	return append(out, toolResults...)
}

// blocksText extracts plain text from a raw value that may be a JSON string
// or an array of content blocks.
func blocksText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []contentBlockWire
	if json.Unmarshal(raw, &blocks) == nil {
		var b strings.Builder
		for _, blk := range blocks {
			if blk.Type == "text" {
				b.WriteString(blk.Text)
			}
		}
		return b.String()
	}
	return ""
}

type contentBlockOut struct {
	Type  string          `json:"type"`
	Text  string          `json:"text,omitempty"`
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

type usageOut struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type messageResponseWire struct {
	ID           string            `json:"id"`
	Type         string            `json:"type"`
	Role         string            `json:"role"`
	Model        string            `json:"model"`
	Content      []contentBlockOut `json:"content"`
	StopReason   string            `json:"stop_reason"`
	StopSequence *string           `json:"stop_sequence"`
	Usage        usageOut          `json:"usage"`
}

// MarshalMessagesResponse renders an llm.ChatResponse as an Anthropic
// Messages response.
func MarshalMessagesResponse(resp llm.ChatResponse) ([]byte, error) {
	out := messageResponseWire{
		ID:    "msg_" + randID(),
		Type:  "message",
		Role:  "assistant",
		Model: resp.Model,
		Usage: usageOut{InputTokens: resp.Usage.PromptTokens, OutputTokens: resp.Usage.CompletionTokens},
	}

	var choice llm.Choice
	if len(resp.Choices) > 0 {
		choice = resp.Choices[0]
	}
	if len(choice.Message.ToolCalls) > 0 {
		for _, tc := range choice.Message.ToolCalls {
			input := tc.Function.Arguments
			if input == "" {
				input = "{}"
			}
			out.Content = append(out.Content, contentBlockOut{
				Type:  "tool_use",
				ID:    tc.ID,
				Name:  tc.Function.Name,
				Input: json.RawMessage(input),
			})
		}
	} else {
		out.Content = append(out.Content, contentBlockOut{Type: "text", Text: choice.Message.Content})
	}
	out.StopReason = StopReason(choice.FinishReason)
	return json.Marshal(out)
}

// StopReason maps an IR finish reason to an Anthropic stop_reason.
func StopReason(finish string) string {
	switch finish {
	case "tool_calls":
		return "tool_use"
	case "length":
		return "max_tokens"
	default:
		return "end_turn"
	}
}

// EstimateInputTokens is a crude rune/4 estimate used for the message_start
// usage in streamed responses (mock fidelity only).
func EstimateInputTokens(req llm.ChatRequest) int {
	n := 0
	for _, m := range req.Messages {
		n += len([]rune(m.Content))
	}
	t := n / 4
	if t < 1 {
		t = 1
	}
	return t
}

func randID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
