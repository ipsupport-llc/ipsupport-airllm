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
	ended     bool // message_delta and message_stop are out
}

// NewStreamWriter builds a StreamWriter. inputTokens seeds the message_start
// usage; outputTokens arrive later via the IR usage chunk.
func NewStreamWriter(w io.Writer, flush func(), id, model string, inputTokens int) *StreamWriter {
	return &StreamWriter{w: w, flush: flush, id: id, model: model, inputTokens: inputTokens}
}

// Chunk consumes one IR stream chunk and emits the corresponding Anthropic
// event(s).
func (s *StreamWriter) Chunk(c llm.StreamChunk) error {
	if s.ended {
		// Nothing may follow message_stop; a later usage report from an
		// upstream that sends several is dropped.
		return nil
	}
	if err := s.start(); err != nil {
		return err
	}
	return s.chunk(c)
}

// start emits message_start once.
func (s *StreamWriter) start() error {
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
	return nil
}

// chunk emits the events for one IR chunk once message_start is out.
func (s *StreamWriter) chunk(c llm.StreamChunk) error {
	// Read the finish reason before dispatching, not inside a case. The cases
	// below are mutually exclusive, so a chunk carrying both a delta and a
	// finish reason would take the delta branch and lose the reason silently —
	// which shipped once, as a Gemini-backed stream whose message_delta had an
	// empty stop_reason. The producer normalises the two apart
	// (providers.yieldSplittingFinish); this makes losing it impossible rather
	// than merely unlikely.
	if c.FinishReason != "" {
		s.stop = StopReason(c.FinishReason)
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
		return s.closeBlock()

	case c.Usage != nil:
		usage := map[string]int{"output_tokens": c.Usage.CompletionTokens}
		if c.Usage.PromptTokens > 0 {
			// message_start's input_tokens was only ever a rune/4 estimate (the
			// real count isn't known until the upstream's own usage arrives,
			// normally in its last chunk) — correct it here now that it's
			// real. Omitted rather than sent as 0 when an upstream never
			// reports prompt tokens at all, so an absent count doesn't read as
			// a confident zero.
			usage["input_tokens"] = c.Usage.PromptTokens
		}
		return s.end(usage)
	}

	// Role-only chunk: message_start already handled above.
	return nil
}

// Finish ends a stream that completed successfully. The terminal events
// normally go out with the upstream's usage chunk; an upstream that reports
// no usage never sends one, and without this the client would wait for a
// message_stop that never comes. A no-op once the message has ended.
func (s *StreamWriter) Finish() error {
	if s.ended {
		return nil
	}
	if err := s.start(); err != nil {
		return err
	}
	return s.end(map[string]int{"output_tokens": 0})
}

// Fail ends a stream that broke after it started with an Anthropic error
// event, the way the real API reports a failure mid-stream: no
// message_delta or message_stop follows, so a client cannot take the
// truncated message for a complete one.
func (s *StreamWriter) Fail(errType, message string) error {
	s.ended = true
	return s.event("error", map[string]any{
		"type":  "error",
		"error": map[string]any{"type": errType, "message": message},
	})
}

// end emits message_delta with usage and then message_stop, exactly once.
func (s *StreamWriter) end(usage map[string]int) error {
	// Anthropic's wire format has no way to close a block after
	// message_delta, so the last chance to close one is here. Normally the
	// finish chunk already did it and this is a no-op; it is not a no-op
	// for a stream that ends without one, and an SDK reading a message
	// whose block never closed is entitled to reject it.
	if err := s.closeBlock(); err != nil {
		return err
	}
	if s.stop == "" {
		s.stop = StopReason("")
	}
	s.ended = true
	if err := s.event("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": s.stop, "stop_sequence": nil},
		"usage": usage,
	}); err != nil {
		return err
	}
	return s.event("message_stop", map[string]any{"type": "message_stop"})
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
