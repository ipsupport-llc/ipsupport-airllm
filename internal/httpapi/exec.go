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
// failed on every fallback tier is a 400 the client can act on; every tier
// quarantined by its breaker is a 503; anything else is a 502 upstream error.
func classifyUpstreamErr(err error) (int, string) {
	if errors.Is(err, errAllBusy) {
		return http.StatusTooManyRequests, "rate_limit_error"
	}
	if errors.Is(err, errAllQuarantined) {
		return http.StatusServiceUnavailable, "upstream_error"
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

// warnUnregisteredTarget logs the one case classifyUpstreamErr can never
// distinguish from an ordinary upstream failure: a provider row that's
// `enabled=true` in Postgres (so routing.Resolve's JOIN offers it as a
// valid target) but failed to build into the LIVE in-memory Registry — a
// malformed kind-specific config logs only at startup/reload time
// (registry_load.go), not when a real request actually hits the gap. If
// every tier hits this, the client just sees a generic 502 with nothing
// to tell an operator the alias is silently broken short of correlating
// an old reload log by hand.
func warnUnregisteredTarget(alias, provider string) {
	slog.Warn("executor: resolved target's provider is not in the live registry",
		"alias", alias, "provider", provider)
}

func (s *Server) freeFunc(reg *providers.Registry) func(string) int {
	return func(name string) int {
		if e, ok := reg.Get(name); ok {
			return e.Free()
		}
		return -1
	}
}

// defaultTokenReservation is the conservative headroom Check reserves
// against a key's token limit when the caller has no better estimate
// (e.g. the client didn't declare max_tokens) — completion length isn't
// knowable before the response, so this is a deliberate, documented
// heuristic ceiling, not a measured value.
const defaultTokenReservation = 4096

// reserveTokensFor picks the token reservation Check should make: the
// client's own declared max_tokens when set (an honest, client-supplied
// worst case), else the default ceiling.
func reserveTokensFor(maxTokens *int) int64 {
	if maxTokens != nil && *maxTokens > 0 {
		return int64(*maxTokens)
	}
	return defaultTokenReservation
}

// limitDenied checks the key's usage limits, optimistically reserving
// reserveTokens of token headroom when the check passes (see
// Limiter.Check's doc) — pass 0 to skip reservation (e.g. for ingresses,
// like audio, with no token dimension). It returns a 429-ready message and
// true when the request must be rejected, plus the Decision so the caller
// can thread its reservation fields through to finalizeUsage/Add. Redis
// errors fail open.
func (s *Server) limitDenied(ctx context.Context, ak authedKey, reserveTokens int64) (string, bool, limits.Decision) {
	dec, err := s.limiter.Check(ctx, ak.KeyID, ak.Policy.ParseLimits(), reserveTokens)
	if err != nil {
		slog.Error("limiter check failed; failing open", "err", err)
		return "", false, limits.Decision{}
	}
	if dec.Allowed {
		return "", false, dec
	}
	return limitMessage(dec), true, limits.Decision{}
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
func (s *Server) finalizeUsage(ctx context.Context, entry ledger.Entry, keyID, upstreamModel string, u llm.Usage, reservedTokens, reservedBucket int64) {
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
		if err := s.limiter.Add(ctx, keyID, u.BilledTokens(), costMicro, 0, 0, reservedTokens, reservedBucket); err != nil {
			slog.Error("limiter add failed", "err", err)
		}
	} else if reservedTokens != 0 {
		// No billable usage (failed request, or a success with zero usage),
		// but Check still reserved headroom optimistically before the
		// outcome was known — refund it now rather than leak it forever.
		if err := s.limiter.Add(ctx, keyID, 0, 0, 0, 0, reservedTokens, reservedBucket); err != nil {
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
		if err := s.limiter.Add(ctx, keyID, 0, costMicro, audioSeconds, ttsChars, 0, 0); err != nil {
			slog.Error("limiter add failed", "err", err)
		}
	}
}

// runChat executes the plan for a unary chat request (see executePlan). On
// total exhaustion the LAST attempted target is returned (not an empty one)
// so the ledger/log can still attribute the failure to a real provider —
// this matters most for the fallback-worthy-error case, where every tier
// gave a real (non-busy) answer.
func (s *Server) runChat(ctx context.Context, plan *routing.Plan, req llm.ChatRequest) (llm.ChatResponse, execResult, error) {
	var resp llm.ChatResponse
	res, _, err := s.executePlan(ctx, plan, nil, func(ctx context.Context, p providers.Provider, t routing.Target, _ func() bool) error {
		var err error
		resp, err = p.Chat(ctx, upstreamRequest(req, t))
		if err == nil && t.Options.ThinkingOff() {
			for i := range resp.Choices {
				resp.Choices[i].Message.Content = stripThinkBlocks(resp.Choices[i].Message.Content)
			}
		}
		return err
	})
	if err != nil {
		return llm.ChatResponse{}, res, err
	}
	return resp, res, nil
}

// streamSink encodes IR stream chunks into a client wire format. begin is
// called exactly once, on the first chunk, so headers are written lazily and
// fallback remains possible until the first byte is sent. It receives the
// target that actually started answering, once known.
type streamSink interface {
	begin(t routing.Target)
	chunk(llm.StreamChunk) error
}

// errFirstChunkLate aborts a stream whose first chunk arrived after its
// target's budget had already run out; runAttempt reports it as a timeout.
var errFirstChunkLate = errors.New("first chunk arrived after the time budget")

// runStream executes the plan for a streaming request (see executePlan).
// The target's time budget bounds the wait for the first chunk; once that
// chunk is emitted the response is committed and a later error cannot be
// recovered (returned with started=true). On total exhaustion the LAST
// attempted target is returned, same reasoning as runChat.
//
// Under a budget, only a chunk carrying text or a tool call counts as the
// first: the role-only and empty deltas a model sends while it thinks are
// held back until real content follows, so a tier that thinks past its budget
// falls through instead of holding the caller silent. Without a budget the
// first chunk of any kind starts the response, as it always has.
func (s *Server) runStream(ctx context.Context, plan *routing.Plan, req llm.ChatRequest, sink streamSink) (served execResult, usage llm.Usage, started bool, err error) {
	served, started, err = s.executePlan(ctx, plan, nil, func(ctx context.Context, p providers.Provider, t routing.Target, commit func() bool) error {
		attemptStarted := false
		var attemptUsage llm.Usage
		var held []llm.StreamChunk
		waitForContent := s.policyFor(t).budget > 0
		var think *thinkFilter
		if t.Options.ThinkingOff() {
			think = &thinkFilter{}
		}
		emit := func(c llm.StreamChunk) error {
			if !attemptStarted {
				if !commit() {
					return errFirstChunkLate
				}
				sink.begin(t)
				attemptStarted = true
				for _, h := range held {
					if err := sink.chunk(h); err != nil {
						return err
					}
				}
				held = nil
			}
			return sink.chunk(c)
		}
		err := p.ChatStream(ctx, upstreamRequest(req, t), func(c llm.StreamChunk) error {
			if think != nil {
				c.Content = think.push(c.Content)
				if c.FinishReason != "" {
					c.Content += think.flush()
				}
			}
			if c.Usage != nil {
				attemptUsage = *c.Usage
			}
			if waitForContent && !attemptStarted && c.Content == "" && len(c.ToolCalls) == 0 {
				held = append(held, c)
				return nil
			}
			return emit(c)
		})
		if err == nil && think != nil {
			if tail := think.flush(); tail != "" {
				err = emit(llm.StreamChunk{Content: tail})
			}
		}
		if err == nil && !attemptStarted && len(held) > 0 {
			// The stream ended without any content: what it did send is the
			// whole answer.
			last := held[len(held)-1]
			held = held[:len(held)-1]
			err = emit(last)
		}
		usage = attemptUsage
		return err
	})
	if err == nil {
		// A stream that ended cleanly without a single chunk still counts
		// as answered: the handler finishes it like any other.
		started = true
	} else if !started {
		usage = llm.Usage{}
	}
	return served, usage, started, err
}

// openaiSink streams OpenAI chat.completion.chunk SSE events and accumulates
// the response text for the capture pipeline.
type openaiSink struct {
	w             http.ResponseWriter
	flush         func()
	meta          openai.StreamMeta
	content       strings.Builder
	exposeBackend bool
	includeUsage  bool // client's own stream_options.include_usage preference
}

func (o *openaiSink) begin(t routing.Target) {
	writeSSEHeaders(o.w, t, o.exposeBackend)
}

func (o *openaiSink) chunk(c llm.StreamChunk) error {
	if c.Content != "" {
		o.content.WriteString(c.Content)
	}
	if c.Usage != nil && !o.includeUsage {
		// This gateway's own billing already captured usage straight from
		// the upstream call (runStream), independent of what reaches the
		// client here — so skipping this chunk only affects what the
		// client sees, matching OpenAI's own default of omitting it unless
		// stream_options.include_usage was set.
		return nil
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

// upstreamRequest builds the provider-facing request for target t: its
// upstream model substituted in, and its thinking setting applied.
func upstreamRequest(req llm.ChatRequest, t routing.Target) llm.ChatRequest {
	out := req
	out.Model = t.UpstreamModel
	out.ThinkingOff = t.Options.ThinkingOff()
	return out
}
