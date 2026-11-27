package httpapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/llm"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/metrics"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/providers"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/routing"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/unavail"
)

// These tests pin the interplay between a target's timeout_ms and the
// provider's own non-streaming ceiling (UPSTREAM_CHAT_TIMEOUT), and that a
// timeout of either kind marks the target unavailable only after
// timeoutsBeforeUnavailable of them in a row.

const testCeiling = 50 * time.Millisecond

// slowUpstream answers every call after delay (a chat completion, or an SSE
// stream when the request asks for one), counting calls.
func slowUpstream(t *testing.T, delay time.Duration) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var d atomic.Int64
	d.Store(int64(delay))
	return variableUpstream(t, &d)
}

// variableUpstream is slowUpstream with a delay that can change between
// calls.
func variableUpstream(t *testing.T, delay *atomic.Int64) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		select {
		case <-r.Context().Done():
			return
		case <-time.After(time.Duration(delay.Load())):
		}
		if strings.Contains(string(body), `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"},\"finish_reason\":null}]}\n\n")
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// slowProvider is an OpenAI-compatible provider named "slow" at up's URL,
// with the ceiling set to testCeiling.
func slowProvider(up *httptest.Server) *providers.OpenAICompat {
	p := providers.NewOpenAICompat("slow", "openai", up.URL, "")
	p.SetChatTimeout(testCeiling)
	return p
}

// soloPlan has the one target and nothing to fall back to, so its failure
// is the request's.
func soloPlan(opts routing.TargetOptions) *routing.Plan {
	return &routing.Plan{
		Alias:    "frog",
		Strategy: "round_robin",
		Tiers:    [][]routing.Target{{{Provider: "slow", UpstreamModel: "m", Options: opts}}},
	}
}

func unaryChat(s *Server, plan *routing.Plan) (execResult, error) {
	_, res, err := s.runChat(context.Background(), plan, llm.ChatRequest{Messages: []llm.Message{{Role: "user", Content: "hi"}}})
	return res, err
}

var slowKey = unavail.Key{Provider: "slow", UpstreamModel: "m"}

// wantTimeout checks err is a timeout with the given code, reads as one in
// the tier-attempt log and to the client, and left the target unmarked.
func wantTimeout(t *testing.T, s *Server, err error, code string) {
	t.Helper()
	var pe *providers.Error
	if !errors.As(err, &pe) || pe.Code != code {
		t.Fatalf("err = %v, want a %s error", err, code)
	}
	if pe.Status != http.StatusGatewayTimeout {
		t.Errorf("status = %d, want 504", pe.Status)
	}
	if r := attemptFailureReason(err); r != "timeout" {
		t.Errorf("attempt failure reason = %q, want timeout", r)
	}
	if m := clientErrorMessage(err, false); m != "upstream did not answer within its time budget" {
		t.Errorf("client message = %q", m)
	}
	if got, _ := classifyUpstreamErr(err); got != http.StatusGatewayTimeout {
		t.Errorf("client status = %d, want 504", got)
	}
	if _, skip := s.unavail.Check(context.Background(), slowKey); skip {
		t.Error("a timed-out target was marked unavailable")
	}
}

func TestUnaryTimeoutMSLongerThanCeilingIsHonoured(t *testing.T) {
	up, calls := slowUpstream(t, 150*time.Millisecond)
	s := newUnavailTestServer(t, newFakeClock(), slowProvider(up))

	res, err := unaryChat(s, soloPlan(routing.TargetOptions{TimeoutMS: ms(5000)}))
	if err != nil {
		t.Fatalf("a 150ms answer under a 5s timeout_ms and a 50ms ceiling failed: %v", err)
	}
	if res.Provider != "slow" || calls.Load() != 1 {
		t.Errorf("served by %q after %d calls, want slow after 1", res.Provider, calls.Load())
	}
}

func TestUnaryGatewayDefaultTimeoutMSReplacesCeiling(t *testing.T) {
	up, _ := slowUpstream(t, 150*time.Millisecond)
	s := newUnavailTestServer(t, newFakeClock(), slowProvider(up))
	s.applyFailover([]byte(`{"timeout_ms":5000}`))

	if _, err := unaryChat(s, soloPlan(routing.TargetOptions{})); err != nil {
		t.Fatalf("a 150ms answer under a 5s default timeout_ms and a 50ms ceiling failed: %v", err)
	}
}

func TestUnaryWithoutTimeoutMSKeepsCeilingAndStaysAvailable(t *testing.T) {
	up, calls := slowUpstream(t, 150*time.Millisecond)
	s := newUnavailTestServer(t, newFakeClock(), slowProvider(up))
	plan := soloPlan(routing.TargetOptions{})

	_, err := unaryChat(s, plan)
	wantTimeout(t, s, err, providers.ErrCodeUpstreamTimeout)

	// No clock advance: a marked target would be skipped without a call.
	_, err = unaryChat(s, plan)
	wantTimeout(t, s, err, providers.ErrCodeUpstreamTimeout)
	if calls.Load() != 2 {
		t.Errorf("upstream called %d times, want 2: a timeout must not shut the target off", calls.Load())
	}
}

func TestUnaryTargetTimeoutMSZeroOverridesDefaultAndKeepsCeiling(t *testing.T) {
	up, _ := slowUpstream(t, 150*time.Millisecond)
	s := newUnavailTestServer(t, newFakeClock(), slowProvider(up))
	s.applyFailover([]byte(`{"timeout_ms":5000}`))

	_, err := unaryChat(s, soloPlan(routing.TargetOptions{TimeoutMS: ms(0)}))
	wantTimeout(t, s, err, providers.ErrCodeUpstreamTimeout)
}

func TestTierTimeoutLeavesTargetAvailable(t *testing.T) {
	up, calls := slowUpstream(t, 500*time.Millisecond)
	p := providers.NewOpenAICompat("slow", "openai", up.URL, "")
	s := newUnavailTestServer(t, newFakeClock(), p)
	plan := soloPlan(routing.TargetOptions{TimeoutMS: ms(50)})

	_, err := unaryChat(s, plan)
	wantTimeout(t, s, err, errCodeTierTimeout)
	_, err = unaryChat(s, plan)
	wantTimeout(t, s, err, errCodeTierTimeout)
	if calls.Load() != 2 {
		t.Errorf("upstream called %d times, want 2: a timeout must not shut the target off", calls.Load())
	}
}

// TestUpstream503StillMarksUnavailable guards the other side: a real
// failure answer keeps marking the target.
func TestUpstream503StillMarksUnavailable(t *testing.T) {
	up := newSwitchableUpstream(t, true)
	s := newUnavailTestServer(t, newFakeClock(), providers.NewOpenAICompat("slow", "openai", up.URL, ""))
	plan := soloPlan(routing.TargetOptions{TimeoutMS: ms(5000)})

	if _, err := unaryChat(s, plan); err == nil || attemptFailureReason(err) != "http_503" {
		t.Fatalf("err = %v, want the upstream's 503", err)
	}
	if _, skip := s.unavail.Check(context.Background(), slowKey); !skip {
		t.Error("a 503 did not mark the target unavailable")
	}
	if _, err := unaryChat(s, plan); !errors.Is(err, errAllUnavailable) {
		t.Errorf("second request err = %v, want %v", err, errAllUnavailable)
	}
	if up.calls.Load() != 1 {
		t.Errorf("upstream called %d times, want 1: a marked target is skipped", up.calls.Load())
	}
}

// TestCeilingTimeoutsStillTripTheBreaker: a target that keeps timing out is
// not marked unavailable, but the breaker still counts each one, so it is
// taken out of rotation by failure rate.
func TestCeilingTimeoutsStillTripTheBreaker(t *testing.T) {
	up, calls := slowUpstream(t, 150*time.Millisecond)
	clk := newFakeClock()
	s := newBreakerTestServer(t, clk, slowProvider(up), providers.NewMock("mock-ok"))
	s.unavail = unavail.New(nil).WithClock(clk.Now)
	plan := guardedPlan("frog", "slow", breakerOn())

	for i := 0; i < 3; i++ {
		if res := chat(t, s, plan); res.Provider != "mock-ok" {
			t.Fatalf("request %d served by %q, want the fallback tier", i, res.Provider)
		}
	}
	if calls.Load() != 3 {
		t.Fatalf("slow upstream called %d times before the trip, want 3 (never marked unavailable)", calls.Load())
	}
	chat(t, s, plan)
	if calls.Load() != 3 {
		t.Errorf("slow upstream called again (%d calls): the breaker must have opened", calls.Load())
	}
}

// TestStreamIgnoresCeiling: a streamed chat never had the non-streaming
// ceiling, with or without a budget.
func TestStreamIgnoresCeiling(t *testing.T) {
	up, _ := slowUpstream(t, 150*time.Millisecond)
	s := newUnavailTestServer(t, newFakeClock(), slowProvider(up))
	req := llm.ChatRequest{Stream: true, Messages: []llm.Message{{Role: "user", Content: "hi"}}}

	for _, opts := range []routing.TargetOptions{{}, {TimeoutMS: ms(5000)}} {
		var content string
		sink := &recordingSink{onChunk: func(c llm.StreamChunk) { content += c.Content }}
		res, _, started, err := s.runStream(context.Background(), soloPlan(opts), req, sink)
		if err != nil || !started || res.Provider != "slow" {
			t.Fatalf("timeout_ms=%v: served by %q started=%v err=%v, want slow", opts.TimeoutMS, res.Provider, started, err)
		}
		if content != "ok" {
			t.Errorf("timeout_ms=%v: content = %q, want ok", opts.TimeoutMS, content)
		}
	}
}

// TestThirdTimeoutInARowMarksUnavailable runs with the breaker off, the
// default: one or two timeouts leave the target in rotation, the third in
// a row marks it, and the next request skips it without a call.
func TestThirdTimeoutInARowMarksUnavailable(t *testing.T) {
	up, calls := slowUpstream(t, 150*time.Millisecond)
	s := newUnavailTestServer(t, newFakeClock(), slowProvider(up))
	plan := soloPlan(routing.TargetOptions{})

	for i := 1; i < timeoutsBeforeUnavailable; i++ {
		_, err := unaryChat(s, plan)
		wantTimeout(t, s, err, providers.ErrCodeUpstreamTimeout)
	}
	if _, err := unaryChat(s, plan); attemptFailureReason(err) != "timeout" {
		t.Fatalf("third request err = %v, want a timeout", err)
	}
	if _, skip := s.unavail.Check(context.Background(), slowKey); !skip {
		t.Fatal("the third timeout in a row did not mark the target unavailable")
	}
	if _, err := unaryChat(s, plan); !errors.Is(err, errAllUnavailable) {
		t.Errorf("fourth request err = %v, want %v", err, errAllUnavailable)
	}
	if calls.Load() != 3 {
		t.Errorf("upstream called %d times, want 3", calls.Load())
	}
}

// TestSuccessResetsTheTimeoutRun: timeouts only count while nothing
// answers in between.
func TestSuccessResetsTheTimeoutRun(t *testing.T) {
	var delay atomic.Int64
	up, calls := variableUpstream(t, &delay)
	s := newUnavailTestServer(t, newFakeClock(), slowProvider(up))
	plan := soloPlan(routing.TargetOptions{})
	timeouts := func(n int) {
		t.Helper()
		delay.Store(int64(150 * time.Millisecond))
		for i := 0; i < n; i++ {
			_, err := unaryChat(s, plan)
			wantTimeout(t, s, err, providers.ErrCodeUpstreamTimeout) // also: still unmarked
		}
	}

	timeouts(2)
	delay.Store(0)
	if _, err := unaryChat(s, plan); err != nil {
		t.Fatalf("fast answer: %v", err)
	}
	timeouts(2)
	if calls.Load() != 5 {
		t.Errorf("upstream called %d times, want 5", calls.Load())
	}
}

// TestFailedTargetIsNotRetriedOnABusyPass: a busy target sends the request
// round the plan again, but a target that already failed in this request is
// not called a second time.
func TestFailedTargetIsNotRetriedOnABusyPass(t *testing.T) {
	up, calls := slowUpstream(t, time.Second)
	reg := providers.NewRegistry()
	reg.Register(providers.NewOpenAICompat("slow", "openai", up.URL, ""), 0)
	reg.Register(providers.NewMock("mock-ok"), 1)
	s := &Server{router: routing.NewRouter(nil), metrics: metrics.New()}
	s.regPtr.Store(reg)
	busy, _ := reg.Get("mock-ok")
	if !busy.Acquire() {
		t.Fatal("could not occupy mock-ok's only slot")
	}
	defer busy.Release()
	plan := budgetPlan(routing.Target{Provider: "slow", UpstreamModel: "m", Options: routing.TargetOptions{TimeoutMS: ms(50)}})

	_, err := unaryChat(s, plan)
	if attemptFailureReason(err) != "timeout" {
		t.Errorf("err = %v, want the slow target's timeout", err)
	}
	if calls.Load() != 1 {
		t.Errorf("slow target called %d times, want 1: it already failed in this request", calls.Load())
	}
}

// TestRunAttemptKeepsAnUpstreamRefusalPastTheBudget: a non-retryable
// upstream answer that comes back as the budget runs out is that answer,
// not a timeout; the budget's own cancellation still is one.
func TestRunAttemptKeepsAnUpstreamRefusalPastTheBudget(t *testing.T) {
	refusal := &providers.Error{Status: http.StatusBadRequest, Message: "upstream slow returned 400: bad request"}
	_, err := runAttempt(context.Background(), "slow", 10*time.Millisecond, func(ctx context.Context, _ func() bool) error {
		<-ctx.Done()
		return refusal
	})
	if err != refusal {
		t.Errorf("err = %v, want the upstream's 400 kept", err)
	}

	_, err = runAttempt(context.Background(), "slow", 10*time.Millisecond, func(ctx context.Context, _ func() bool) error {
		<-ctx.Done()
		return ctx.Err()
	})
	var pe *providers.Error
	if !errors.As(err, &pe) || pe.Code != errCodeTierTimeout {
		t.Errorf("err = %v, want the budget's cancellation relabelled tier_timeout", err)
	}
}
