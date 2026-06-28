package httpapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/rromenskyi/ipsupport-airllm/internal/ledger"
	"github.com/rromenskyi/ipsupport-airllm/internal/limits"
	"github.com/rromenskyi/ipsupport-airllm/internal/llm"
	"github.com/rromenskyi/ipsupport-airllm/internal/openai"
	"github.com/rromenskyi/ipsupport-airllm/internal/providers"
	"github.com/rromenskyi/ipsupport-airllm/internal/routing"
)

// limitDenied checks the key's usage limits. It returns a 429-ready message
// and true when the request must be rejected. Redis errors fail open.
func (s *Server) limitDenied(ctx context.Context, ak authedKey) (string, bool) {
	dec, err := s.limiter.Check(ctx, ak.KeyID, ak.Policy.ParseLimits())
	if err != nil {
		slog.Error("limiter check failed; failing open", "err", err)
		return "", false
	}
	if dec.Allowed {
		return "", false
	}
	return limitMessage(dec), true
}

func limitMessage(d limits.Decision) string {
	if d.Unit == "cost_usd" {
		return fmt.Sprintf("usage limit exceeded: cost over %s ($%.4f used, $%.4f cap)",
			d.Window, float64(d.Used)/1e6, float64(d.Limit)/1e6)
	}
	return fmt.Sprintf("usage limit exceeded: %s over %s (%d used, %d cap)",
		d.Unit, d.Window, d.Used, d.Limit)
}

// finalizeUsage computes cost, fills the ledger entry, records it, and (on a
// successful request with non-zero usage) increments the rolling counters.
func (s *Server) finalizeUsage(ctx context.Context, entry ledger.Entry, keyID, upstreamModel string, prompt, completion int) {
	entry.PromptTokens = prompt
	entry.CompletionTokens = completion
	costMicro := s.pricing.CostMicroUSD(upstreamModel, prompt, completion)
	entry.CostUSD = float64(costMicro) / 1e6
	s.ledger.Record(ctx, entry)

	if entry.Status == http.StatusOK && (prompt > 0 || completion > 0) {
		if err := s.limiter.Add(ctx, keyID, int64(prompt+completion), costMicro); err != nil {
			slog.Error("limiter add failed", "err", err)
		}
	}
}

// runChat tries targets in priority order, returning the first successful
// response and the target that served it. A retryable provider error
// advances to the next target; a non-retryable error aborts.
func (s *Server) runChat(ctx context.Context, targets []routing.Target, req llm.ChatRequest) (llm.ChatResponse, routing.Target, error) {
	var lastErr error
	for _, t := range targets {
		prov, ok := s.providers.Get(t.Provider)
		if !ok {
			lastErr = fmt.Errorf("provider %q not registered", t.Provider)
			continue
		}
		resp, err := prov.Chat(ctx, upstreamRequest(req, t.UpstreamModel))
		if err == nil {
			return resp, t, nil
		}
		lastErr = err
		if !providers.IsRetryable(err) {
			return llm.ChatResponse{}, t, err
		}
	}
	if lastErr == nil {
		lastErr = errors.New("no targets to try")
	}
	return llm.ChatResponse{}, routing.Target{}, lastErr
}

// streamSink encodes IR stream chunks into a client wire format. begin is
// called exactly once, on the first chunk, so headers are written lazily and
// fallback remains possible until the first byte is sent.
type streamSink interface {
	begin()
	chunk(llm.StreamChunk) error
}

// runStream tries targets in order. Fallback is only possible before the
// first chunk is emitted; once streaming starts, a later error is returned
// with started=true and cannot be recovered. usage holds the last usage
// reported by the served target.
func (s *Server) runStream(ctx context.Context, targets []routing.Target, req llm.ChatRequest, sink streamSink) (served routing.Target, usage llm.Usage, started bool, err error) {
	var lastErr error
	for _, t := range targets {
		prov, ok := s.providers.Get(t.Provider)
		if !ok {
			lastErr = fmt.Errorf("provider %q not registered", t.Provider)
			continue
		}

		attemptStarted := false
		var attemptUsage llm.Usage
		callErr := prov.ChatStream(ctx, upstreamRequest(req, t.UpstreamModel), func(c llm.StreamChunk) error {
			if !attemptStarted {
				sink.begin()
				attemptStarted = true
			}
			if c.Usage != nil {
				attemptUsage = *c.Usage
			}
			return sink.chunk(c)
		})

		if callErr == nil {
			return t, attemptUsage, true, nil
		}
		lastErr = callErr
		if attemptStarted {
			return t, attemptUsage, true, callErr
		}
		if !providers.IsRetryable(callErr) {
			return t, llm.Usage{}, false, callErr
		}
	}
	if lastErr == nil {
		lastErr = errors.New("no targets to try")
	}
	return routing.Target{}, llm.Usage{}, false, lastErr
}

// openaiSink streams OpenAI chat.completion.chunk SSE events.
type openaiSink struct {
	w     http.ResponseWriter
	flush func()
	meta  openai.StreamMeta
}

func (o *openaiSink) begin() {
	writeSSEHeaders(o.w)
}

func (o *openaiSink) chunk(c llm.StreamChunk) error {
	b, err := openai.MarshalStreamChunk(o.meta, c)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(o.w, "data: %s\n\n", b); err != nil {
		return err
	}
	o.flush()
	return nil
}

func writeSSEHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
}

// upstreamRequest builds the provider-facing request with the resolved
// upstream model substituted in.
func upstreamRequest(req llm.ChatRequest, upstreamModel string) llm.ChatRequest {
	out := req
	out.Model = upstreamModel
	return out
}
