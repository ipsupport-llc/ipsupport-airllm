package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/llm"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/providers"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/routing"
)

// A target with stream_upstream serves a unary chat from a stream: the
// gateway asks its upstream for a stream, assembles the chunks into one
// ordinary response and answers the client with that, so the client sees no
// difference. What it buys is the upstream's progress. A unary call shows
// nothing until the whole answer is there, so a hung model cannot be told
// from a slow one before the whole budget has run out; a stream shows every
// token. The target's budget (timeout_ms) then bounds the wait for the first
// output, and the stall timeout (stall_timeout_ms) every silence after it;
// nothing else bounds the whole answer once output flows. Nothing reaches
// the client until the stream has ended, so any failure along the way — a
// stall, an error event, a broken or cut-short stream — still falls through
// to the next target, as a unary failure would.

// errCodeUpstreamStall marks a stream_upstream attempt abandoned because its
// upstream, having started answering, went quiet for longer than its stall
// timeout. A timeout like tier_timeout (see isTimeout): retryable, it counts
// against the tier and towards the run of timeouts that marks a target
// unavailable.
const errCodeUpstreamStall = "upstream_stall"

// streamsUpstream reports whether a unary chat request for a target whose
// policy asks for stream_upstream can be served from a stream. A request
// whose answer a stream would not carry the same way — several choices, or
// log probabilities — is served by a plain unary call. (The OpenAI ingress
// already refuses n other than 1; the check stands for any other way in.)
func streamsUpstream(pol attemptPolicy, req llm.ChatRequest) bool {
	if !pol.streamUpstream {
		return false
	}
	if raw, ok := req.Extra["n"]; ok {
		var n *float64
		if json.Unmarshal(raw, &n) != nil || (n != nil && *n != 1) {
			return false
		}
	}
	for _, k := range []string{"logprobs", "top_logprobs"} {
		if raw, ok := req.Extra[k]; ok {
			if v := strings.TrimSpace(string(raw)); v != "null" && v != "false" {
				return false
			}
		}
	}
	return true
}

// Why streamChat cancelled its own call.
const (
	streamRunning int32 = iota
	streamCeilingHit
	streamStalled
)

// streamChat serves one unary chat attempt for target t from a stream (see
// above). req is the provider-facing request (upstreamRequest); the provider
// shapes it exactly as it shapes a unary one and asks for usage on the
// stream.
//
// The first chunk with output — text, thinking or a tool call; a role-only
// or empty chunk is not one, so a long prompt read is not taken for a stall
// — reports progress to runAttempt, which stops the target's budget there.
// A call without a budget is bounded up to that chunk by the provider's own
// non-streaming ceiling (providers.ChatCeiling), as its unary call would
// have been, and fails as that ceiling's upstream_timeout. From the first
// output on, a gap longer than stall between two chunks with output abandons
// the attempt with an upstream_stall timeout.
//
// The answer is accepted only when the upstream said it is complete — [DONE]
// or a finish reason of its own (providers.WithRequireStreamEnd); a stream
// that just stops, a raw error after the stream started (the connection
// broke mid-body) and a chunk of a second choice are retryable upstream
// failures, so the next target is tried.
//
// When the stream reports no usage, or none for a side that clearly had
// some, the response carries an estimate for it (see estimateMissingUsage)
// and a warning is logged: billing zero would let the request past every
// token budget.
func streamChat(ctx context.Context, p providers.Provider, t routing.Target, req llm.ChatRequest, stall time.Duration, gate attemptGate) (llm.ChatResponse, error) {
	parent := ctx
	ctx, cancel := context.WithCancel(providers.WithRequireStreamEnd(ctx))
	defer cancel()
	var why atomic.Int32
	var timer *time.Timer
	var ceiling time.Duration
	if _, ok := providers.CallBudget(ctx); !ok {
		ceiling = providers.ChatCeiling(p)
		timer = time.AfterFunc(ceiling, func() {
			if why.CompareAndSwap(streamRunning, streamCeilingHit) {
				cancel()
			}
		})
	}
	live := liveFrom(ctx)
	var acc llm.StreamAccumulator
	var chunks, outputs int
	err := p.ChatStream(ctx, req, func(c llm.StreamChunk) error {
		chunks++
		if c.OtherChoice {
			return &providers.Error{Status: http.StatusBadGateway, Retryable: true,
				Message: fmt.Sprintf("upstream %s answered with more than one choice", t.Provider)}
		}
		live.observeChunk(c)
		acc.Add(c)
		if !c.HasOutput() {
			return nil
		}
		outputs++
		if why.Load() != streamRunning {
			return context.Canceled
		}
		if outputs > 1 {
			if timer != nil {
				timer.Reset(stall)
			}
			return nil
		}
		if timer != nil && !timer.Stop() {
			// The ceiling ran out just now: claim it here, so the call is
			// not left to look like a broken stream while the timer's own
			// goroutine has yet to run.
			why.CompareAndSwap(streamRunning, streamCeilingHit)
			cancel()
			return context.Canceled
		}
		timer = nil
		if !gate.progress() {
			return errFirstChunkLate
		}
		timer = time.AfterFunc(stall, func() {
			if why.CompareAndSwap(streamRunning, streamStalled) {
				cancel()
			}
		})
		return nil
	})
	if timer != nil {
		timer.Stop()
	}
	if err != nil {
		switch state := why.Load(); {
		case parent.Err() != nil:
			// The client went away or the budget ran out (runAttempt
			// relabels the latter): either way not this call's verdict.
		case state == streamStalled:
			err = &providers.Error{
				Status:    http.StatusGatewayTimeout,
				Retryable: true,
				Code:      errCodeUpstreamStall,
				Message:   fmt.Sprintf("upstream %s stalled: no output for %s after it started answering", t.Provider, stall),
			}
		case state == streamCeilingHit:
			err = &providers.Error{
				Status:    http.StatusGatewayTimeout,
				Retryable: true,
				Code:      providers.ErrCodeUpstreamTimeout,
				Message:   fmt.Sprintf("upstream %s did not start answering within %s (UPSTREAM_CHAT_TIMEOUT)", t.Provider, ceiling),
			}
		case chunks > 0 && !errors.Is(err, errFirstChunkLate) && !isProviderError(err):
			err = &providers.Error{
				Status:    http.StatusBadGateway,
				Retryable: true,
				Message:   fmt.Sprintf("upstream %s stream broke: %v", t.Provider, err),
			}
		}
		return llm.ChatResponse{}, err
	}
	resp := acc.Response()
	if len(resp.Choices) == 0 {
		return resp, nil // runChat fails it as an answer with no choices
	}
	reported, _ := acc.Usage()
	if u, estimated := estimateMissingUsage(reported, req, &acc, outputs > 0); estimated {
		slog.Warn("upstream stream reported no usage (or none for one side); the response carries an estimate",
			"provider", t.Provider, "upstream_model", t.UpstreamModel,
			"reported_prompt_tokens", reported.PromptTokens, "reported_completion_tokens", reported.CompletionTokens,
			"prompt_tokens", u.PromptTokens, "completion_tokens", u.CompletionTokens)
		resp.Usage = u
	}
	resp.ID = "chatcmpl-" + newID()
	resp.Model = req.Model
	resp.Created = time.Now().Unix()
	return resp, nil
}

// isProviderError reports whether err is already a classified provider
// failure.
func isProviderError(err error) bool {
	var pe *providers.Error
	return errors.As(err, &pe)
}

// estimateMissingUsage fills in the usage a stream did not report, by the
// gateway's rune/4 estimate (llm.EstimateTokens): a prompt reported as 0 (or
// not at all) is estimated over the whole request — messages, earlier tool
// calls and the tool schemas (llm.EstimatePromptTokens) — and a completion
// reported as 0 when output was seen is estimated over everything the model
// produced: text, thinking (also the reasoning share) and tool calls' names
// and arguments. A side the upstream did count is kept. estimated is whether
// anything was filled in.
func estimateMissingUsage(u llm.Usage, req llm.ChatRequest, acc *llm.StreamAccumulator, sawOutput bool) (_ llm.Usage, estimated bool) {
	if u.PromptTokens == 0 {
		u.PromptTokens = llm.EstimatePromptTokens(req)
		estimated = true
	}
	if u.CompletionTokens == 0 && sawOutput {
		all, reasoning := acc.OutputText()
		u.CompletionTokens = max(llm.EstimateTokens(all), 1)
		u.ReasoningTokens = llm.EstimateTokens(reasoning)
		estimated = true
	}
	if estimated {
		u.TotalTokens = u.PromptTokens + u.CompletionTokens
		u.Normalize()
	}
	return u, estimated
}
