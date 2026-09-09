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
// 429 (back off and retry); a recognized context-length/model-not-found/
// multimodal-not-supported/reasoning-effort-unsupported error that still
// failed on every fallback tier is a 400 the client can act on; anything
// else is a 502 upstream error.
func classifyUpstreamErr(err error) (int, string) {
	if errors.Is(err, errAllBusy) {
		return http.StatusTooManyRequests, "rate_limit_error"
	}
	var pe *providers.Error
	if errors.As(err, &pe) {
		switch pe.Code {
		case providers.ErrCodeContextLengthExceeded, providers.ErrCodeModelNotFound, providers.ErrCodeMultimodalNotSupported, providers.ErrCodeReasoningEffortUnsupported:
			return http.StatusBadRequest, "invalid_request_error"
		}
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
//
// It takes the whole llm.Usage rather than a pair of counts because that type
// carries the invariant the money depends on: CompletionTokens is everything
// billed at the output rate, thinking included, and ReasoningTokens is the
// share of it that was thinking. Cost and the rolling caps therefore read the
// same two numbers they always did and are right for a reasoning model without
// knowing what one is; the reasoning count rides along for the ledger, the
// metrics and the log line, where an operator can see the split.
func (s *Server) finalizeUsage(ctx context.Context, entry ledger.Entry, keyID, upstreamModel string, u llm.Usage) {
	entry.PromptTokens = u.PromptTokens
	entry.CompletionTokens = u.CompletionTokens
	entry.ReasoningTokens = u.ReasoningTokens
	costMicro := s.pricing.CostMicroUSD(entry.ProviderName, upstreamModel, u.PromptTokens, u.CompletionTokens)
	entry.CostUSD = float64(costMicro) / 1e6
	s.ledger.Record(ctx, entry)
	s.metrics.RecordUsage(entry.IngressProtocol, u, entry.CostUSD)

	logAttrs := []any{
		"alias", entry.Alias, "provider", entry.ProviderName, "upstream_model", upstreamModel,
		"ingress", entry.IngressProtocol, "status", entry.Status,
		"prompt_tokens", u.PromptTokens, "completion_tokens", u.CompletionTokens,
		"cost_usd", entry.CostUSD, "latency_ms", entry.LatencyMS,
	}
	// Logged only when there is one, so the shape of every existing log line
	// is untouched and a search for the attribute finds reasoning traffic.
	if u.ReasoningTokens > 0 {
		logAttrs = append(logAttrs, "reasoning_tokens", u.ReasoningTokens)
	}
	if entry.ErrorMsg != "" {
		slog.Error("request completed", append(logAttrs, "error", entry.ErrorMsg)...)
	} else {
		slog.Info("request completed", logAttrs...)
	}

	if entry.Status == http.StatusOK && (u.PromptTokens > 0 || u.CompletionTokens > 0) {
		if err := s.limiter.Add(ctx, keyID, u.BilledTokens(), costMicro, 0, 0); err != nil {
			slog.Error("limiter add failed", "err", err)
		}
	}
}

// finalizeAudioUsage records an audio request's usage the same way
// finalizeUsage does for chat: it fills cost onto the ledger entry, records
// it, updates the RecordUsage cost metric, and emits the same "request
// completed" structured log line, so audio requests are visible in
// Prometheus/Grafana and log-based tooling exactly like chat requests are.
// Unlike finalizeUsage, the limiter increment is not gated on
// entry.Status == 200: a transcription DLP-blocks AFTER the real upstream
// call already ran (see dlpScanText's call site in api_audio.go), so a
// blocked response can still carry real, billable usage that must count
// against both cost_usd and audio_seconds/tts_chars caps. Add already
// no-ops when every quantity is zero, so gating on "any non-zero quantity"
// (rather than status) correctly skips the Redis round trip for requests
// that never reached a provider.
func (s *Server) finalizeAudioUsage(ctx context.Context, entry ledger.Entry, keyID string, costMicro, audioSeconds, ttsChars int64) {
	entry.CostUSD = float64(costMicro) / 1e6
	s.ledger.Record(ctx, entry)
	s.metrics.RecordUsage(entry.IngressProtocol, llm.Usage{}, entry.CostUSD)

	logAttrs := []any{
		"alias", entry.Alias, "provider", entry.ProviderName, "upstream_model", entry.UpstreamModel,
		"ingress", entry.IngressProtocol, "status", entry.Status,
		"audio_seconds", audioSeconds, "tts_chars", ttsChars,
		"cost_usd", entry.CostUSD, "latency_ms", entry.LatencyMS,
	}
	if entry.ErrorMsg != "" {
		slog.Error("request completed", append(logAttrs, "error", entry.ErrorMsg)...)
	} else {
		slog.Info("request completed", logAttrs...)
	}

	if costMicro != 0 || audioSeconds != 0 || ttsChars != 0 {
		if err := s.limiter.Add(ctx, keyID, 0, costMicro, audioSeconds, ttsChars); err != nil {
			slog.Error("limiter add failed", "err", err)
		}
	}
}

// runChat executes the plan: it walks the tiers (each ordered by the alias
// strategy), acquiring a concurrency slot per attempt. A busy target is
// skipped; a retryable or fallback-worthy error advances to the next
// target; if every target is busy it waits briefly and retries. On total
// exhaustion, the LAST attempted target is returned (not an empty one) so
// the ledger/log can still attribute the failure to a real provider —
// this matters most for the fallback-worthy-error case this function
// exists to handle, where every tier gave a real (non-busy) answer.
func (s *Server) runChat(ctx context.Context, plan *routing.Plan, req llm.ChatRequest) (llm.ChatResponse, routing.Target, error) {
	reg := s.reg()
	free := s.freeFunc(reg)
	var lastErr error
	var lastTarget routing.Target

	for attempt := 0; attempt <= busyRetries; attempt++ {
		anyBusy := false
		for _, t := range plan.Ordered(s.router.NextRR(plan.Alias), free) {
			lastTarget = t
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
			if !providers.IsFallbackWorthy(err) {
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
	if errors.Is(lastErr, errAllBusy) {
		s.metrics.IncRateLimited("provider_busy")
	}
	return llm.ChatResponse{}, lastTarget, lastErr
}

// streamSink encodes IR stream chunks into a client wire format. begin is
// called exactly once, on the first chunk, so headers are written lazily and
// fallback remains possible until the first byte is sent. It receives the
// target that actually started answering, once known.
type streamSink interface {
	begin(t routing.Target)
	chunk(llm.StreamChunk) error
}

// runStream executes the plan for a streaming request. Concurrency slots,
// tier fallback, and the busy-retry wait mirror runChat; but once the first
// chunk is emitted the response is committed and a later error cannot be
// recovered (returned with started=true). On total exhaustion the LAST
// attempted target is returned, same reasoning as runChat.
func (s *Server) runStream(ctx context.Context, plan *routing.Plan, req llm.ChatRequest, sink streamSink) (served routing.Target, usage llm.Usage, started bool, err error) {
	reg := s.reg()
	free := s.freeFunc(reg)
	var lastErr error
	var lastTarget routing.Target

	for attempt := 0; attempt <= busyRetries; attempt++ {
		anyBusy := false
		for _, t := range plan.Ordered(s.router.NextRR(plan.Alias), free) {
			lastTarget = t
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
					sink.begin(t)
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
			if !providers.IsFallbackWorthy(callErr) {
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
	if errors.Is(lastErr, errAllBusy) {
		s.metrics.IncRateLimited("provider_busy")
	}
	return lastTarget, llm.Usage{}, false, lastErr
}

// openaiSink streams OpenAI chat.completion.chunk SSE events and accumulates
// the response text for the capture pipeline.
type openaiSink struct {
	w             http.ResponseWriter
	flush         func()
	meta          openai.StreamMeta
	content       strings.Builder
	exposeBackend bool
}

func (o *openaiSink) begin(t routing.Target) {
	writeSSEHeaders(o.w, t, o.exposeBackend)
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

func writeSSEHeaders(w http.ResponseWriter, t routing.Target, exposeBackend bool) {
	if exposeBackend && t.DisplayLabel != "" {
		w.Header().Set("X-Backend-Model", t.DisplayLabel)
	}
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
