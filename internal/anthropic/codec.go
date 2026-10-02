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
	"log/slog"
	"strings"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/llm"
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
	// image — base64 (inline) or url (Anthropic's remote-fetch source)
	Source *struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type,omitempty"`
		Data      string `json:"data,omitempty"`
		URL       string `json:"url,omitempty"`
	} `json:"source,omitempty"`
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
	if w.MaxTokens <= 0 {
		// Unlike OpenAI, Anthropic's real Messages API makes max_tokens a
		// required, positive field — a request missing it (or sending
		// 0/negative) gets a clean 400 from the real API, not a silent
		// gateway-chosen default.
		return llm.ChatRequest{}, errors.New("max_tokens is required and must be positive")
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

	mt := w.MaxTokens
	req := llm.ChatRequest{
		Model:       w.Model,
		Messages:    msgs,
		Tools:       tools,
		ToolChoice:  translateToolChoice(w.ToolChoice),
		MaxTokens:   &mt,
		Temperature: w.Temperature,
		Stream:      w.Stream,
	}
	return req, nil
}

// translateToolChoice converts an Anthropic-shaped tool_choice into the IR's
// OpenAI-shaped form. Every real upstream in this codebase is reached
// through the OpenAI-shaped client regardless of which protocol the client
// used (see package doc) — forwarding Anthropic's tool_choice syntax
// (`{"type":"any"}`, `{"type":"tool","name":"x"}`) to an OpenAI-shaped API
// unmodified would send it a field shape it doesn't understand. Shapes this
// function doesn't recognize are dropped (not forwarded raw), the same
// fail-safe choice this file already makes for unsupported image sources.
func translateToolChoice(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var tc struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &tc); err != nil {
		slog.Warn("anthropic tool_choice malformed; dropping", "err", err)
		return nil
	}
	switch tc.Type {
	case "auto":
		return json.RawMessage(`"auto"`)
	case "any":
		return json.RawMessage(`"required"`)
	case "tool":
		if tc.Name == "" {
			slog.Warn("anthropic tool_choice type=tool missing name; dropping")
			return nil
		}
		b, _ := json.Marshal(map[string]any{
			"type":     "function",
			"function": map[string]string{"name": tc.Name},
		})
		return b
	default:
		slog.Warn("anthropic tool_choice has unrecognized type; dropping", "type", tc.Type)
		return nil
	}
}

// convertMessage maps one Anthropic message to one or more IR messages. A
// message bearing tool_result blocks splits those into IR "tool" messages,
// emitted in their original position relative to any surrounding text/image/
// tool_use blocks — a user turn can legitimately interleave tool_result
// blocks with new text (e.g. two tool results plus added commentary), and
// that relative order matters to the upstream model.
func convertMessage(mw messageWire) []llm.Message {
	var s string
	if json.Unmarshal(mw.Content, &s) == nil {
		return []llm.Message{{Role: mw.Role, Content: s}}
	}

	var blocks []contentBlockWire
	if json.Unmarshal(mw.Content, &blocks) != nil {
		return []llm.Message{{Role: mw.Role}}
	}

	var out []llm.Message
	var texts []string
	var images []llm.Image
	var toolCalls []llm.ToolCall

	// flush emits the accumulated text/image/tool_use blocks as one message,
	// in place, before a tool_result forces a split (or at the end of the
	// loop). A no-op when nothing has accumulated, so adjacent tool_results
	// don't produce empty messages between them.
	flush := func() {
		if len(texts) == 0 && len(images) == 0 && len(toolCalls) == 0 {
			return
		}
		out = append(out, llm.Message{
			Role:      mw.Role,
			Content:   strings.Join(texts, ""),
			Images:    images,
			ToolCalls: toolCalls,
		})
		texts, images, toolCalls = nil, nil, nil
	}

	for _, blk := range blocks {
		switch blk.Type {
		case "text":
			texts = append(texts, blk.Text)
		case "image":
			if img, ok := imageFromBlock(blk, mw.Role); ok {
				images = append(images, img)
			}
		case "tool_use":
			args := string(blk.Input)
			if args == "" {
				args = "{}"
			}
			toolCalls = append(toolCalls, llm.ToolCall{
				ID:       blk.ID,
				Type:     "function",
				Function: llm.FunctionCall{Name: blk.Name, Arguments: args},
			})
		case "tool_result":
			flush()
			text, toolImages := blocksTextAndImages(blk.Content, "tool")
			out = append(out, llm.Message{
				Role:       "tool",
				ToolCallID: blk.ToolUseID,
				Content:    text,
				Images:     toolImages,
			})
		}
	}
	flush()
	return out
}

// imageFromBlock converts a content block's image source into an llm.Image,
// or reports false — having already logged why — when it isn't a usable
// image block.
func imageFromBlock(blk contentBlockWire, role string) (llm.Image, bool) {
	switch {
	case blk.Source == nil:
		slog.Warn("anthropic image block missing source; dropping", "role", role)
	case blk.Source.Type == "base64" && blk.Source.MediaType != "" && blk.Source.Data != "":
		return llm.Image{URL: "data:" + blk.Source.MediaType + ";base64," + blk.Source.Data}, true
	case blk.Source.Type == "base64":
		slog.Warn("anthropic base64 image block missing media_type or data; dropping", "role", role)
	case blk.Source.Type == "url" && blk.Source.URL != "":
		return llm.Image{URL: blk.Source.URL}, true
	case blk.Source.Type == "url":
		slog.Warn("anthropic url image block missing url; dropping", "role", role)
	default:
		slog.Warn("anthropic image block has unsupported source type; dropping", "role", role, "source_type", blk.Source.Type)
	}
	return llm.Image{}, false
}

// blocksText extracts plain text from a raw value that may be a JSON string
// or an array of content blocks (used for the system prompt, which Anthropic
// never allows to carry images).
func blocksText(raw json.RawMessage) string {
	text, _ := blocksTextAndImages(raw, "")
	return text
}

// blocksTextAndImages extracts text and images from a raw value that may be
// a JSON string (no images possible) or an array of content blocks — used
// for tool_result content, which Anthropic allows to carry both text and
// image blocks, same as a top-level user turn.
func blocksTextAndImages(raw json.RawMessage, role string) (string, []llm.Image) {
	if len(raw) == 0 {
		return "", nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, nil
	}
	var blocks []contentBlockWire
	if json.Unmarshal(raw, &blocks) == nil {
		var b strings.Builder
		var images []llm.Image
		for _, blk := range blocks {
			switch blk.Type {
			case "text":
				b.WriteString(blk.Text)
			case "image":
				if img, ok := imageFromBlock(blk, role); ok {
					images = append(images, img)
				}
			}
		}
		return b.String(), images
	}
	return "", nil
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
	// Text and tool_use are independent, not mutually exclusive: a model can
	// emit lead-in commentary alongside a tool call in the same turn (e.g.
	// "Let me check that." + a tool_use). This used to be an if/else keyed
	// on ToolCalls, which silently dropped Content whenever tool calls were
	// also present. A message with no tool calls still gets a text block
	// even when Content is empty, matching the prior always-text-when-no-
	// tools behavior.
	if choice.Message.Content != "" || len(choice.Message.ToolCalls) == 0 {
		out.Content = append(out.Content, contentBlockOut{Type: "text", Text: choice.Message.Content})
	}
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
// usage in streamed responses, sent before any upstream — real or mock —
// has reported real prompt-token usage. StreamWriter corrects it once the
// real count arrives, in the final message_delta's usage.input_tokens.
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
