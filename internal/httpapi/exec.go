package httpapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/ledger"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/limits"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/llm"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/openai"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/providers"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/routing"
)

// errAllBusy is returned when every target is at its concurrency cap.
var errAllBusy = errors.New("all upstreams are at capacity")

// Bounded wait when all targets are momentarily saturated.
const (
	busyRetries = 4
	busyBackoff = 40 * time.Millisecond
)

// classifyUpstreamErr maps an executor error to an HTTP status: all-busy is a
// 429 (back off and retry), anything else is a 502 upstream error.
func classifyUpstreamErr(err error) (int, string) {
	if errors.Is(err, errAllBusy) {
		return http.StatusTooManyRequests, "rate_limit_error"
	}
	return http.StatusBadGateway, "upstream_error"
}

func (s *Server) freeFunc(reg *providers.Registry) func(string) int {
	return func(name string) int {
		if e, ok := reg.Get(name); ok {
			return e.Free()
		}
		return -1
	}
}

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

// runChat executes the plan: it walks the tiers (each ordered by the alias
// strategy), acquiring a concurrency slot per attempt. A busy target is
// skipped; a retryable error advances to the next target; if every target is
// busy it waits briefly and retries, finally returning errAllBusy.
func (s *Server) runChat(ctx context.Context, plan *routing.Plan, req llm.ChatRequest) (llm.ChatResponse, routing.Target, error) {
	reg := s.reg()
	free := s.freeFunc(reg)
	var lastErr error

	for attempt := 0; attempt <= busyRetries; attempt++ {
		anyBusy := false
		for _, t := range plan.Ordered(s.router.NextRR(plan.Alias), free) {
			e, ok := reg.Get(t.Provider)
			if !ok {
				lastErr = fmt.Errorf("provider %q not registered", t.Provider)
				continue
			}
			if !e.Acquire() {
				anyBusy = true
				continue
			}
			resp, err := e.Provider.Chat(ctx, upstreamRequest(req, t.UpstreamModel))
			e.Release()
			if err == nil {
				return resp, t, nil
			}
			lastErr = err
			if !providers.IsRetryable(err) {
				return llm.ChatResponse{}, t, err
			}
		}
		if !anyBusy {
			break
		}
		select {
		case <-ctx.Done():
			return llm.ChatResponse{}, routing.Target{}, ctx.Err()
		case <-time.After(busyBackoff):
		}
	}
	if lastErr == nil {
		lastErr = errAllBusy
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

// runStream executes the plan for a streaming request. Concurrency slots,
// tier fallback, and the busy-retry wait mirror runChat; but once the first
// chunk is emitted the response is committed and a later error cannot be
// recovered (returned with started=true).
func (s *Server) runStream(ctx context.Context, plan *routing.Plan, req llm.ChatRequest, sink streamSink) (served routing.Target, usage llm.Usage, started bool, err error) {
	reg := s.reg()
	free := s.freeFunc(reg)
	var lastErr error

	for attempt := 0; attempt <= busyRetries; attempt++ {
		anyBusy := false
		for _, t := range plan.Ordered(s.router.NextRR(plan.Alias), free) {
			e, ok := reg.Get(t.Provider)
			if !ok {
				lastErr = fmt.Errorf("provider %q not registered", t.Provider)
				continue
			}
			if !e.Acquire() {
				anyBusy = true
				continue
			}

			attemptStarted := false
			var attemptUsage llm.Usage
			callErr := e.Provider.ChatStream(ctx, upstreamRequest(req, t.UpstreamModel), func(c llm.StreamChunk) error {
				if !attemptStarted {
					sink.begin()
					attemptStarted = true
				}
				if c.Usage != nil {
					attemptUsage = *c.Usage
				}
				return sink.chunk(c)
			})
			e.Release()

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
		if !anyBusy {
			break
		}
		select {
		case <-ctx.Done():
			return routing.Target{}, llm.Usage{}, false, ctx.Err()
		case <-time.After(busyBackoff):
		}
	}
	if lastErr == nil {
		lastErr = errAllBusy
	}
	return routing.Target{}, llm.Usage{}, false, lastErr
}

// openaiSink streams OpenAI chat.completion.chunk SSE events and accumulates
// the response text for the capture pipeline.
type openaiSink struct {
	w       http.ResponseWriter
	flush   func()
	meta    openai.StreamMeta
	content strings.Builder
}

func (o *openaiSink) begin() {
	writeSSEHeaders(o.w)
}

func (o *openaiSink) chunk(c llm.StreamChunk) error {
	if c.Content != "" {
		o.content.WriteString(c.Content)
	}
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

// assembled returns the full accumulated response text.
func (o *openaiSink) assembled() string { return o.content.String() }

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
