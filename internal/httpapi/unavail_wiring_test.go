package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/providers"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/routing"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/unavail"
)

// newUnavailTestServer wires s.unavail with a fake clock instead of real
// Redis — deterministic, and lets tests advance time past a backoff window
// without sleeping.
func newUnavailTestServer(t *testing.T, clk *fakeClock, ps ...providers.Provider) *Server {
	t.Helper()
	s := newRunChatTestServer(t, ps...)
	s.unavail = unavail.New(nil).WithClock(clk.Now)
	return s
}

// TestUnavailableTargetSkipsSubsequentRequestsThenRetries is the Minor
// fix's core mechanic end to end: a tier-attributable failure marks the
// (provider, model) unavailable; the NEXT request skips it without a call
// (no fallback to the client — it moves straight to the next tier); after
// the backoff window elapses, a later request tries it again, proving
// there's no separate synthetic prober — the next real request IS the
// retry.
func TestUnavailableTargetSkipsSubsequentRequestsThenRetries(t *testing.T) {
	up := newSwitchableUpstream(t, true) // starts failing with a 503, no Retry-After
	clk := newFakeClock()
	s := newUnavailTestServer(t, clk, providers.NewOpenAICompat("flaky", "openai", up.URL, ""), providers.NewMock("mock-ok"))
	plan := guardedPlan("voice-reply", "flaky", routing.TargetOptions{})

	if res := chat(t, s, plan); res.Provider != "mock-ok" {
		t.Fatalf("request 1 served by %q, want the fallback tier (flaky must fail first)", res.Provider)
	}
	if up.calls.Load() != 1 {
		t.Fatalf("flaky called %d times after request 1, want 1", up.calls.Load())
	}

	// Default backoff is 200ms (the built-in default, since no failover
	// settings were saved for this test Server) — well within the window,
	// flaky must be skipped entirely, not called again.
	clk.Advance(50 * time.Millisecond)
	if res := chat(t, s, plan); res.Provider != "mock-ok" {
		t.Fatalf("request 2 served by %q, want the fallback tier", res.Provider)
	}
	if up.calls.Load() != 1 {
		t.Fatalf("flaky called %d times after request 2 (still within the backoff window), want 1 (skipped)", up.calls.Load())
	}

	// Past the window: the next real request is itself the retry.
	clk.Advance(200 * time.Millisecond)
	up.failing.Store(false) // flaky has recovered
	if res := chat(t, s, plan); res.Provider != "flaky" {
		t.Fatalf("request 3 served by %q, want flaky (the backoff window elapsed, so this request retries it)", res.Provider)
	}
	if up.calls.Load() != 2 {
		t.Fatalf("flaky called %d times after request 3, want 2 (the retry)", up.calls.Load())
	}
}

// TestUnavailableHonorsRetryAfterHeader proves the upstream's own
// Retry-After is used exactly, not the gateway's default backoff: a
// shorter Retry-After than the default lets a retry happen SOONER than the
// 200ms default would.
func TestUnavailableHonorsRetryAfterHeader(t *testing.T) {
	var retrying bool
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !retrying {
			w.Header().Set("Retry-After", "60") // seconds — far longer than the 200ms default
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"message":"overloaded"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"c","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	t.Cleanup(up.Close)

	clk := newFakeClock()
	s := newUnavailTestServer(t, clk, providers.NewOpenAICompat("flaky", "openai", up.URL, ""), providers.NewMock("mock-ok"))
	plan := guardedPlan("voice-reply", "flaky", routing.TargetOptions{})

	if res := chat(t, s, plan); res.Provider != "mock-ok" {
		t.Fatalf("request 1 served by %q, want the fallback tier", res.Provider)
	}

	// Past the 200ms DEFAULT but nowhere near the 60s Retry-After: must
	// still be skipped, proving the header value — not the default — won.
	clk.Advance(5 * time.Second)
	if res := chat(t, s, plan); res.Provider != "mock-ok" {
		t.Fatalf("request 2 (5s later) served by %q, want still skipped (Retry-After said 60s)", res.Provider)
	}

	clk.Advance(56 * time.Second) // total 61s, past the 60s Retry-After
	retrying = true
	if res := chat(t, s, plan); res.Provider != "flaky" {
		t.Fatalf("request 3 (61s later) served by %q, want flaky (Retry-After's 60s has elapsed)", res.Provider)
	}
}

// TestUnavailableMarkIsPerProviderModelNotPerAliasTier proves the scoping
// decision this feature was built around: two DIFFERENT aliases whose
// tier 0 both point at the exact same (provider, model) share the mark —
// a failure discovered via one alias immediately protects the other,
// without needing its own independent failure run first.
func TestUnavailableMarkIsPerProviderModelNotPerAliasTier(t *testing.T) {
	up := newSwitchableUpstream(t, true)
	clk := newFakeClock()
	s := newUnavailTestServer(t, clk, providers.NewOpenAICompat("flaky", "openai", up.URL, ""), providers.NewMock("mock-ok"))

	planA := guardedPlan("alias-a", "flaky", routing.TargetOptions{})
	planB := guardedPlan("alias-b", "flaky", routing.TargetOptions{})

	if res := chat(t, s, planA); res.Provider != "mock-ok" {
		t.Fatalf("alias-a served by %q, want the fallback tier", res.Provider)
	}
	if up.calls.Load() != 1 {
		t.Fatalf("flaky called %d times after alias-a's request, want 1", up.calls.Load())
	}

	// alias-b has never been used before — but its tier 0 is the SAME
	// (provider, model) alias-a just marked unavailable.
	if res := chat(t, s, planB); res.Provider != "mock-ok" {
		t.Fatalf("alias-b served by %q, want the fallback tier", res.Provider)
	}
	if up.calls.Load() != 1 {
		t.Fatalf("flaky called %d times after alias-b's request, want still 1 (the mark from alias-a must protect alias-b too)", up.calls.Load())
	}
}
