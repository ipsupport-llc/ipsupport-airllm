package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/breaker"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/llm"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/providers"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/routing"
)

// These are the seam tests for the per-tier circuit breaker. Requests go
// through the executor into real providers talking to in-process upstreams;
// the assertions are on which upstream was called, who answered and what the
// client got. Cooldowns and windows run on an injected clock, so no test
// sleeps for anything like a production duration.

// fakeClock is a settable clock for the breaker.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{now: time.Unix(1_700_000_000, 0)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// switchableUpstream answers chat completions with a 503 while failing is
// set and with a short completion otherwise, counting every call.
type switchableUpstream struct {
	*httptest.Server
	calls   atomic.Int32
	failing atomic.Bool
}

func newSwitchableUpstream(t *testing.T, failing bool) *switchableUpstream {
	t.Helper()
	u := &switchableUpstream{}
	u.failing.Store(failing)
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if u.failing.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"message":"overloaded"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"c","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	t.Cleanup(u.Close)
	return u
}

// breakerOn is the per-tier option that switches the breaker on with the
// gateway-wide default thresholds.
func breakerOn() routing.TargetOptions {
	return routing.TargetOptions{Breaker: &routing.BreakerOptions{Enabled: boolp(true)}}
}

// newBreakerTestServer is newRunChatTestServer with a breaker on the given
// clock and no Redis: the local state store.
func newBreakerTestServer(t *testing.T, clk *fakeClock, ps ...providers.Provider) *Server {
	t.Helper()
	s := newRunChatTestServer(t, ps...)
	s.breaker = s.newBreaker(nil, breaker.WithClock(clk.Now))
	return s
}

// guardedPlan puts a breaker-guarded tier 0 above a healthy mock tier 1.
func guardedPlan(alias, provider string, opts routing.TargetOptions) *routing.Plan {
	return &routing.Plan{
		Alias:    alias,
		Strategy: "round_robin",
		Tiers: [][]routing.Target{
			{{Provider: provider, UpstreamModel: "m", Options: opts, Tier: 0}},
			{{Provider: "mock-ok", UpstreamModel: "mock-ok-model", Tier: 1}},
		},
	}
}

func chat(t *testing.T, s *Server, plan *routing.Plan) execResult {
	t.Helper()
	_, res, err := s.runChat(context.Background(), plan, llm.ChatRequest{Messages: []llm.Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("runChat: %v", err)
	}
	return res
}

func TestBreakerOpensAfterConsecutiveFailuresAndSkipsTheTier(t *testing.T) {
	up := newSwitchableUpstream(t, true)
	clk := newFakeClock()
	s := newBreakerTestServer(t, clk, providers.NewOpenAICompat("flaky", "openai", up.URL, ""), providers.NewMock("mock-ok"))
	plan := guardedPlan("voice-reply", "flaky", breakerOn())

	for i := 0; i < 3; i++ {
		if res := chat(t, s, plan); res.Provider != "mock-ok" {
			t.Fatalf("request %d served by %q, want the fallback tier", i, res.Provider)
		}
	}
	if up.calls.Load() != 3 {
		t.Fatalf("flaky upstream called %d times before the trip, want 3", up.calls.Load())
	}

	res := chat(t, s, plan)
	if up.calls.Load() != 3 {
		t.Errorf("flaky upstream called again (%d calls) — an open tier must be skipped", up.calls.Load())
	}
	if res.Provider != "mock-ok" || res.Attempts != 1 {
		t.Errorf("served=%q attempts=%d, want mock-ok on the only attempt", res.Provider, res.Attempts)
	}
}

// breakerWith switches the breaker on with the given overrides.
func breakerWith(o routing.BreakerOptions) routing.TargetOptions {
	o.Enabled = boolp(true)
	return routing.TargetOptions{Breaker: &o}
}

// tripTier fails the guarded tier until it opens, under the default run of
// three consecutive failures.
func tripTier(t *testing.T, s *Server, plan *routing.Plan, up *switchableUpstream) {
	t.Helper()
	up.failing.Store(true)
	for i := 0; i < 3; i++ {
		chat(t, s, plan)
	}
}

// servedByGuarded reports whether the next request reaches the guarded tier's
// upstream at all, whichever tier ends up answering.
func servedByGuarded(t *testing.T, s *Server, plan *routing.Plan, up *switchableUpstream) bool {
	t.Helper()
	before := up.calls.Load()
	chat(t, s, plan)
	return up.calls.Load() > before
}

func TestBreakerOpensOnErrorRateOnceTheWindowHasVolume(t *testing.T) {
	up := newSwitchableUpstream(t, false)
	clk := newFakeClock()
	s := newBreakerTestServer(t, clk, providers.NewOpenAICompat("flaky", "openai", up.URL, ""), providers.NewMock("mock-ok"))
	plan := guardedPlan("voice-reply", "flaky", breakerOn())

	// fail, ok, fail, ok: never two failures in a row, 2 of 4 failed.
	for _, fail := range []bool{true, false, true, false} {
		up.failing.Store(fail)
		chat(t, s, plan)
	}
	up.failing.Store(true)
	chat(t, s, plan) // fifth request: 3 of 5 failed, over half
	if servedByGuarded(t, s, plan, up) {
		t.Fatal("tier still called after more than half of five requests failed")
	}
}

func TestBreakerErrorRateCountsOnlyTheCurrentWindow(t *testing.T) {
	up := newSwitchableUpstream(t, false)
	clk := newFakeClock()
	s := newBreakerTestServer(t, clk, providers.NewOpenAICompat("flaky", "openai", up.URL, ""), providers.NewMock("mock-ok"))
	plan := guardedPlan("voice-reply", "flaky", breakerOn())

	for _, fail := range []bool{true, false, true} {
		up.failing.Store(fail)
		chat(t, s, plan)
	}
	clk.Advance(31 * time.Second) // the window rolls over: its two failures are forgotten
	for _, fail := range []bool{false, true} {
		up.failing.Store(fail)
		chat(t, s, plan)
	}
	if !servedByGuarded(t, s, plan, up) {
		t.Fatal("tier opened on failures spread over two windows")
	}
}

func TestBreakerProbesAfterTheCooldownAndDoublesItOnAFailedProbe(t *testing.T) {
	up := newSwitchableUpstream(t, true)
	clk := newFakeClock()
	s := newBreakerTestServer(t, clk, providers.NewOpenAICompat("flaky", "openai", up.URL, ""), providers.NewMock("mock-ok"))
	plan := guardedPlan("voice-reply", "flaky", breakerOn())
	tripTier(t, s, plan, up)

	clk.Advance(time.Minute - time.Millisecond)
	if servedByGuarded(t, s, plan, up) {
		t.Fatal("tier called before its 60s cooldown ran out")
	}
	clk.Advance(time.Millisecond)
	if !servedByGuarded(t, s, plan, up) {
		t.Fatal("no probe after the 60s cooldown")
	}
	// The probe failed: open again, now for 120s.
	clk.Advance(2*time.Minute - time.Millisecond)
	if servedByGuarded(t, s, plan, up) {
		t.Fatal("tier called before the doubled 120s cooldown ran out")
	}
	clk.Advance(time.Millisecond)
	up.failing.Store(false)
	before := up.calls.Load()
	if res := chat(t, s, plan); res.Provider != "flaky" || up.calls.Load() != before+1 {
		t.Fatalf("probe after 120s: served=%q calls=%d, want the recovered tier to answer", res.Provider, up.calls.Load()-before)
	}
	// The probe succeeded: closed, every request goes to the tier again.
	for i := 0; i < 3; i++ {
		if res := chat(t, s, plan); res.Provider != "flaky" {
			t.Fatalf("request %d after a successful probe served by %q, want the closed tier", i, res.Provider)
		}
	}
}

func TestBreakerCooldownStopsAtTheCeiling(t *testing.T) {
	up := newSwitchableUpstream(t, true)
	clk := newFakeClock()
	s := newBreakerTestServer(t, clk, providers.NewOpenAICompat("flaky", "openai", up.URL, ""), providers.NewMock("mock-ok"))
	plan := guardedPlan("voice-reply", "flaky", breakerWith(routing.BreakerOptions{CooldownMS: ms(1000), MaxCooldownMS: ms(3000)}))
	tripTier(t, s, plan, up)

	// Each failed probe re-opens for 1s, 2s, then 3s (the ceiling), 3s.
	for i, cooldown := range []time.Duration{time.Second, 2 * time.Second, 3 * time.Second, 3 * time.Second} {
		clk.Advance(cooldown - time.Millisecond)
		if servedByGuarded(t, s, plan, up) {
			t.Fatalf("trip %d: tier called before its %v cooldown ran out", i+1, cooldown)
		}
		clk.Advance(time.Millisecond)
		if !servedByGuarded(t, s, plan, up) {
			t.Fatalf("trip %d: no probe after its %v cooldown", i+1, cooldown)
		}
	}
}

func TestBreakerCooldownResetsAfterTheStablePeriod(t *testing.T) {
	up := newSwitchableUpstream(t, true)
	clk := newFakeClock()
	s := newBreakerTestServer(t, clk, providers.NewOpenAICompat("flaky", "openai", up.URL, ""), providers.NewMock("mock-ok"))
	plan := guardedPlan("voice-reply", "flaky", breakerOn())

	reopenAndClose := func() {
		tripTier(t, s, plan, up)
		clk.Advance(time.Minute)
		chat(t, s, plan) // failed probe: next cooldown 120s
		clk.Advance(2 * time.Minute)
		up.failing.Store(false)
		chat(t, s, plan) // successful probe: closed
	}
	reopenAndClose()

	// Ten stable minutes later the history is forgotten: the next trip is
	// back to the initial 60s.
	clk.Advance(10 * time.Minute)
	tripTier(t, s, plan, up)
	clk.Advance(time.Minute)
	if !servedByGuarded(t, s, plan, up) {
		t.Fatal("no probe 60s after a trip that followed ten stable minutes — the cooldown did not reset")
	}
}

func TestBreakerAdmitsASingleProbe(t *testing.T) {
	release := make(chan struct{})
	var calls atomic.Int32
	var holding atomic.Bool
	entered := make(chan struct{}, 1)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if !holding.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		entered <- struct{}{}
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(up.Close)
	clk := newFakeClock()
	s := newBreakerTestServer(t, clk, providers.NewOpenAICompat("flaky", "openai", up.URL, ""), providers.NewMock("mock-ok"))
	plan := guardedPlan("voice-reply", "flaky", breakerOn())
	for i := 0; i < 3; i++ {
		chat(t, s, plan)
	}
	clk.Advance(time.Minute)

	holding.Store(true)
	probe := make(chan execResult)
	go func() { probe <- chat(t, s, plan) }()
	<-entered
	// While the probe is out, everyone else skips the tier.
	for i := 0; i < 3; i++ {
		if res := chat(t, s, plan); res.Provider != "mock-ok" {
			t.Fatalf("request during the probe served by %q, want the fallback tier", res.Provider)
		}
	}
	close(release)
	if res := <-probe; res.Provider != "flaky" {
		t.Errorf("probe served by %q, want the probed tier", res.Provider)
	}
	if got := calls.Load(); got != 4 {
		t.Errorf("upstream called %d times, want 3 failures and one probe", got)
	}
}

func TestBreakerScopeIsTheAliasTierNotTheProvider(t *testing.T) {
	up := newSwitchableUpstream(t, true)
	clk := newFakeClock()
	s := newBreakerTestServer(t, clk, providers.NewOpenAICompat("shared", "openai", up.URL, ""), providers.NewMock("mock-ok"))
	voice := guardedPlan("voice-reply", "shared", breakerOn())
	text := guardedPlan("text-chat", "shared", breakerOn())
	tripTier(t, s, voice, up)

	up.failing.Store(false)
	if servedByGuarded(t, s, voice, up) {
		t.Fatal("the tripped alias still calls its tier")
	}
	if res := chat(t, s, text); res.Provider != "shared" {
		t.Errorf("another alias on the same provider served by %q, want the provider itself", res.Provider)
	}
}

func TestBreakerFailsFastWhenEveryTierIsQuarantined(t *testing.T) {
	up := newSwitchableUpstream(t, true)
	clk := newFakeClock()
	s := newBreakerTestServer(t, clk, providers.NewOpenAICompat("only", "openai", up.URL, ""))
	plan := &routing.Plan{Alias: "single", Strategy: "round_robin", Tiers: [][]routing.Target{
		{{Provider: "only", UpstreamModel: "m", Options: breakerOn()}},
	}}
	req := llm.ChatRequest{Messages: []llm.Message{{Role: "user", Content: "hi"}}}
	for i := 0; i < 3; i++ {
		_, _, _ = s.runChat(context.Background(), plan, req)
	}

	_, res, err := s.runChat(context.Background(), plan, req)
	if up.calls.Load() != 3 || res.Attempts != 0 {
		t.Errorf("calls=%d attempts=%d, want the open tier skipped", up.calls.Load(), res.Attempts)
	}
	if code, _ := classifyUpstreamErr(err); code != http.StatusServiceUnavailable {
		t.Errorf("error %v maps to %d, want 503", err, code)
	}
}

func TestBreakerIgnoresFailuresTheRequestCaused(t *testing.T) {
	clk := newFakeClock()
	s := newBreakerTestServer(t, clk, providers.NewMock("mock-ctxfail"), providers.NewMock("mock-ok"))
	plan := &routing.Plan{Alias: "text", Strategy: "round_robin", Tiers: [][]routing.Target{
		{{Provider: "mock-ctxfail", UpstreamModel: "mock-ctxfail-model", Options: breakerOn()}},
		{{Provider: "mock-ok", UpstreamModel: "mock-ok-model", Tier: 1}},
	}}
	for i := 0; i < 5; i++ {
		if res := chat(t, s, plan); res.Attempts != 2 {
			t.Fatalf("request %d: attempts=%d, want the context-length tier tried every time", i, res.Attempts)
		}
	}
}

func TestBreakerIsOffUnlessEnabled(t *testing.T) {
	up := newSwitchableUpstream(t, true)
	clk := newFakeClock()
	s := newBreakerTestServer(t, clk, providers.NewOpenAICompat("flaky", "openai", up.URL, ""), providers.NewMock("mock-ok"))
	plan := guardedPlan("voice-reply", "flaky", routing.TargetOptions{})
	for i := 0; i < 6; i++ {
		chat(t, s, plan)
	}
	if up.calls.Load() != 6 {
		t.Errorf("tier called %d of 6 times with no breaker configured, want every time", up.calls.Load())
	}

	// The gateway-wide default switches it on for every tier.
	s.failoverPtr.Store(&failoverConfig{Breaker: routing.BreakerOptions{Enabled: boolp(true)}})
	tripTier(t, s, plan, up)
	if servedByGuarded(t, s, plan, up) {
		t.Error("tier still called with the breaker enabled gateway-wide")
	}
}
