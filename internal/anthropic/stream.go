package anthropic

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/llm"
)

// StreamWriter translates the IR stream-chunk sequence into Anthropic
// Messages SSE events: message_start, content_block_start/delta/stop,
// message_delta, message_stop.
type StreamWriter struct {
	w           io.Writer
	flush       func()
	id          string
	model       string
	inputTokens int

	started   bool
	blockKind string // "" | "text" | "tool"
	blockIdx  int    // client-facing index of the open block
	blockTool int    // upstream tool-call index the open tool block belongs to
	nextIdx   int    // next client-facing block index
	stop      string
}

// NewStreamWriter builds a StreamWriter. inputTokens seeds the message_start
// usage; outputTokens arrive later via the IR usage chunk.
func NewStreamWriter(w io.Writer, flush func(), id, model string, inputTokens int) *StreamWriter {
	return &StreamWriter{w: w, flush: flush, id: id, model: model, inputTokens: inputTokens}
}

// Chunk consumes one IR stream chunk and emits the corresponding Anthropic
// event(s).
func (s *StreamWriter) Chunk(c llm.StreamChunk) error {
	if !s.started {
		if err := s.event("message_start", map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id":            s.id,
				"type":          "message",
				"role":          "assistant",
				"model":         s.model,
				"content":       []any{},
				"stop_reason":   nil,
				"stop_sequence": nil,
				"usage":         map[string]int{"input_tokens": s.inputTokens, "output_tokens": 0},
			},
		}); err != nil {
			return err
		}
		s.started = true
	}

	switch {
	case len(c.ToolCalls) > 0:
		for _, tc := range c.ToolCalls {
			if err := s.ensureToolBlock(tc); err != nil {
				return err
			}
			if tc.Function.Arguments == "" {
				continue // head chunk; a filler here would corrupt the JSON
			}
			if err := s.event("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": s.blockIdx,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": tc.Function.Arguments},
			}); err != nil {
				return err
			}
		}
		return nil

	case c.Content != "":
		if s.blockKind != "text" {
			if err := s.closeBlock(); err != nil {
				return err
			}
			if err := s.event("content_block_start", map[string]any{
				"type":          "content_block_start",
				"index":         s.nextIdx,
				"content_block": map[string]any{"type": "text", "text": ""},
			}); err != nil {
				return err
			}
			s.blockKind, s.blockIdx = "text", s.nextIdx
			s.nextIdx++
		}
		return s.event("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": s.blockIdx,
			"delta": map[string]any{"type": "text_delta", "text": c.Content},
		})

	case c.FinishReason != "":
		s.stop = StopReason(c.FinishReason)
		return s.closeBlock()

	case c.Usage != nil:
		if err := s.event("message_delta", map[string]any{
			"type":  "message_delta",
			"delta": map[string]any{"stop_reason": s.stop, "stop_sequence": nil},
			"usage": map[string]int{"output_tokens": c.Usage.CompletionTokens},
		}); err != nil {
			return err
		}
		return s.event("message_stop", map[string]any{"type": "message_stop"})
	}

	// Role-only chunk: message_start already handled above.
	return nil
}

// ensureToolBlock opens a tool_use block for this delta's upstream index,
// closing whatever block was open, unless it is already the open block.
// Assumes one call's deltas arrive contiguously (how OpenAI-compatible
// upstreams stream); interleaved indexes would fork a spurious block, and
// Anthropic's wire format cannot reopen a closed block without buffering.
func (s *StreamWriter) ensureToolBlock(tc llm.ToolCallDelta) error {
	if s.blockKind == "tool" && s.blockTool == tc.Index {
		return nil
	}
	if err := s.closeBlock(); err != nil {
		return err
	}
	if err := s.event("content_block_start", map[string]any{
		"type":  "content_block_start",
		"index": s.nextIdx,
		"content_block": map[string]any{
			"type": "tool_use", "id": tc.ID, "name": tc.Function.Name, "input": map[string]any{},
		},
	}); err != nil {
		return err
	}
	s.blockKind, s.blockIdx, s.blockTool = "tool", s.nextIdx, tc.Index
	s.nextIdx++
	return nil
}

// closeBlock emits content_block_stop for the open block, if any.
func (s *StreamWriter) closeBlock() error {
	if s.blockKind == "" {
		return nil
	}
	if err := s.event("content_block_stop", map[string]any{"type": "content_block_stop", "index": s.blockIdx}); err != nil {
		return err
	}
	s.blockKind = ""
	return nil
}

func (s *StreamWriter) event(name string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", name, b); err != nil {
		return err
	}
	s.flush()
	return nil
}
