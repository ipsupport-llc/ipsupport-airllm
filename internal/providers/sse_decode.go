package providers

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/llm"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/openai"
)

// debugUpstreamSSE logs every upstream request body and raw SSE line when
// DEBUG_UPSTREAM_SSE=1. Diagnostic only — never enable where prompts must
// stay out of logs.
var debugUpstreamSSE = os.Getenv("DEBUG_UPSTREAM_SSE") == "1"

// sendChatCompletions posts an encoded body to an OpenAI-shaped
// /chat/completions endpoint and hands back a 2xx response for the caller to
// decode; the caller closes its body.
//
// It is the shared request half of what decodeSSEStream is the shared
// response half of. Both things a caller must not get wrong live here: a
// transport failure is retryable (the next attempt may well connect), and a
// non-2xx goes through httpError so the vendor's error envelope is
// classified in one place rather than per provider.
//
// bearer is omitted when empty, which is how a local upstream with no
// authentication is addressed.
func sendChatCompletions(ctx context.Context, hc *http.Client, name, baseURL, bearer string, body []byte, stream bool) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}

	resp, err := hc.Do(req)
	if err != nil {
		return nil, &Error{Status: http.StatusBadGateway, Retryable: true, Message: err.Error()}
	}
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		resp.Body.Close()
		return nil, httpError(name, resp.StatusCode, b)
	}
	return resp, nil
}

// streamDecodeOptions tunes decodeSSEStream for one upstream's quirks.
type streamDecodeOptions struct {
	// provider names the upstream in log lines. Diagnostics only.
	provider string

	// coalesceUsage holds usage back and reports it once, after the finish
	// signal, instead of forwarding every usage-bearing chunk as it arrives.
	//
	// Vendors differ here. OpenAI, xAI and Groq report usage once, at the end,
	// so forwarding each report is the same as forwarding one — leave this off
	// for them. Vertex AI reports *cumulative* usage on many chunks, and the
	// Anthropic-shaped egress emits an end-of-message event for every usage
	// chunk it sees, so forwarding each one would end that stream early and
	// then repeatedly.
	coalesceUsage bool
}

// decodeSSEStream reads an OpenAI-shaped SSE chat-completions body and yields
// the IR chunks it carries, in order. It is the shared streaming loop: any
// provider speaking the OpenAI wire format calls it with its own options
// rather than reimplementing the decode.
//
// Whatever the upstream does, the sequence it emits obeys the llm.StreamChunk
// contract: deltas, then exactly one finish-reason chunk — synthesized if the
// upstream never sent one — and usage always on a chunk of its own, never
// riding along with a delta. How many usage chunks come out is the upstream's
// business and coalesceUsage's: one per report when it is off, one in total
// when it is on, none either way if the upstream reported no usage.
//
// If yield returns an error the stream stops and that error is returned.
func decodeSSEStream(body io.Reader, opts streamDecodeOptions, yield func(llm.StreamChunk) error) error {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var sawFinish, sawToolCalls bool
	var coalescedUsage *llm.Usage
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(line[len("data:"):])
		if data == "" {
			continue
		}
		if debugUpstreamSSE {
			slog.Info("upstream sse", "provider", opts.provider, "data", data)
		}
		if data == "[DONE]" {
			break
		}
		chunk, err := openai.ParseStreamChunk([]byte(data))
		if err != nil {
			// skip a malformed chunk rather than abort the stream, but
			// leave a trace: silent drops have hidden real defects.
			slog.Warn("dropping malformed upstream chunk", "provider", opts.provider, "err", err)
			continue
		}
		if chunk.FinishReason != "" {
			sawFinish = true
		}
		if len(chunk.ToolCalls) > 0 {
			sawToolCalls = true
		}
		if chunk.Usage == nil {
			if err := yield(chunk); err != nil {
				return err
			}
			continue
		}
		// Usage is present. Either way it is split off the chunk it rode in
		// on, because the IR documents usage as its own chunk and every egress
		// relies on that shape.
		usage := chunk.Usage
		chunk.Usage = nil
		switch {
		case opts.coalesceUsage:
			// The reports are cumulative, so the last one wins, and it goes
			// out after the loop once the finish signal is clear. Nothing is
			// emitted for it here.
			coalescedUsage = usage
			usage = nil
		case !sawFinish:
			// Two upstream quirks land here, both from Groq: (1) finish_reason
			// and usage arrive bundled in the SAME message instead of separate
			// chunks like OpenAI/xAI send, and (2) finish_reason is never
			// populated at all. Synthesize the missing finish_reason in the
			// correct position — before usage.
			chunk.FinishReason = synthesizedFinishReason(sawToolCalls)
			sawFinish = true
		}
		if hasPayload(chunk) {
			if err := yield(chunk); err != nil {
				return err
			}
		}
		if usage != nil {
			if err := yield(llm.StreamChunk{Usage: usage}); err != nil {
				return err
			}
		}
	}
	if err := sc.Err(); err != nil {
		// The stream broke mid-flight. Any coalesced usage is dropped along
		// with the finish signal rather than flushed: the only way out of here
		// is yield, and a usage chunk reaching the Anthropic egress ends the
		// message — so flushing would dress a truncated response up as a
		// complete one to buy partial token counts. Not a coalescing-only
		// loss, either: an upstream that reports usage once, at the end, has
		// not sent it yet at this point.
		return err
	}
	if !sawFinish {
		if err := yield(llm.StreamChunk{FinishReason: synthesizedFinishReason(sawToolCalls)}); err != nil {
			return err
		}
	}
	if coalescedUsage != nil {
		if err := yield(llm.StreamChunk{Usage: coalescedUsage}); err != nil {
			return err
		}
	}
	return nil
}

// hasPayload reports whether a chunk still carries something a client needs
// once usage has been split off it — an all-zero remainder is dropped rather
// than yielded as an empty chunk.
func hasPayload(c llm.StreamChunk) bool {
	return c.Role != "" || c.Content != "" || len(c.ToolCalls) > 0 || c.FinishReason != ""
}

// synthesizedFinishReason picks the finish reason to fabricate for an
// upstream that never sent one.
func synthesizedFinishReason(sawToolCalls bool) string {
	if sawToolCalls {
		return "tool_calls"
	}
	return "stop"
}
