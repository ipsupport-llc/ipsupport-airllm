package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/breaker"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/llm"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/providers"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/routing"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/store"
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

// testBreakerRedis connects to TEST_REDIS_URL or skips.
func testBreakerRedis(t *testing.T) *redis.Client {
	t.Helper()
	dsn := os.Getenv("TEST_REDIS_URL")
	if dsn == "" {
		t.Skip("TEST_REDIS_URL not set; skipping shared breaker state test")
	}
	opt, err := redis.ParseURL(dsn)
	if err != nil {
		t.Fatalf("parse redis url: %v", err)
	}
	rdb := redis.NewClient(opt)
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

func TestBreakerStateIsSharedThroughRedis(t *testing.T) {
	rdb := testBreakerRedis(t)
	up := newSwitchableUpstream(t, true)
	clk := newFakeClock()
	newReplica := func() *Server {
		s := newRunChatTestServer(t, providers.NewOpenAICompat("flaky", "openai", up.URL, ""), providers.NewMock("mock-ok"))
		s.breaker = s.newBreaker(rdb, breaker.WithClock(clk.Now))
		return s
	}
	a, b := newReplica(), newReplica()
	plan := guardedPlan(fmt.Sprintf("shared-%d", time.Now().UnixNano()), "flaky", breakerOn())

	tripTier(t, a, plan, up)
	if servedByGuarded(t, b, plan, up) {
		t.Fatal("replica b still calls a tier replica a opened")
	}

	// After the cooldown exactly one of the two replicas gets the probe.
	clk.Advance(time.Minute)
	up.failing.Store(false)
	before := up.calls.Load()
	chat(t, b, plan)
	chat(t, a, plan)
	if got := up.calls.Load() - before; got != 2 {
		t.Fatalf("upstream called %d times after the cooldown, want b's probe and then a's normal request", got)
	}
}

func TestBreakerKeepsWorkingWithoutRedis(t *testing.T) {
	// Nothing listens on this address: every Redis call fails at once.
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 50 * time.Millisecond, MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	up := newSwitchableUpstream(t, true)
	clk := newFakeClock()
	s := newRunChatTestServer(t, providers.NewOpenAICompat("flaky", "openai", up.URL, ""), providers.NewMock("mock-ok"))
	s.breaker = s.newBreaker(rdb, breaker.WithClock(clk.Now))
	plan := guardedPlan("voice-reply", "flaky", breakerOn())

	tripTier(t, s, plan, up)
	start := time.Now()
	if servedByGuarded(t, s, plan, up) {
		t.Fatal("tier still called — the local fallback state did not open it")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("a request with Redis down took %v", elapsed)
	}
}

type aliasHealthBody struct {
	Alias string `json:"alias"`
	Tiers []struct {
		Tier                int       `json:"tier"`
		BreakerEnabled      bool      `json:"breaker_enabled"`
		State               string    `json:"state"`
		Reason              string    `json:"reason"`
		OpenUntil           time.Time `json:"open_until"`
		CooldownMS          int64     `json:"cooldown_ms"`
		ConsecutiveFailures int       `json:"consecutive_failures"`
		Targets             []struct {
			Provider string `json:"provider"`
		} `json:"targets"`
	} `json:"tiers"`
}

func getAliasHealth(t *testing.T, s *Server, alias string) (int, aliasHealthBody) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/admin/aliases/"+alias+"/health", nil)
	req.SetPathValue("alias", alias)
	rec := httptest.NewRecorder()
	s.handleAdminAliasHealth(rec, req)
	var body aliasHealthBody
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body
}

// TestAdminAliasHealthAndRelease opens a tier through real traffic, reads it
// back through the admin health view and releases it by hand.
func TestAdminAliasHealthAndRelease(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	suffix := time.Now().UnixNano()
	prov, okProv := fmt.Sprintf("health-flaky-%d", suffix), fmt.Sprintf("health-ok-%d", suffix)
	alias := fmt.Sprintf("health-alias-%d", suffix)
	for _, p := range []string{prov, okProv} {
		if _, err := pool.Exec(ctx, `INSERT INTO providers (name, kind, base_url, enabled, max_concurrency) VALUES ($1, 'openai', 'http://127.0.0.1:1', true, 1)`, p); err != nil {
			t.Fatalf("seed provider: %v", err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM model_aliases WHERE alias = $1`, alias)
		_, _ = pool.Exec(context.Background(), `DELETE FROM providers WHERE name = ANY($1)`, []string{prov, okProv})
	})
	up := newSwitchableUpstream(t, true)
	clk := newFakeClock()
	s := newBreakerTestServer(t, clk, providers.NewOpenAICompat(prov, "openai", up.URL, ""), providers.NewMock(okProv))
	s.st = &store.Store{PG: pool}
	s.router = routing.NewRouter(s.st)
	var audited []string
	s.auditHook = func(_ context.Context, _, action, _ string, _ any) { audited = append(audited, action) }

	body := fmt.Sprintf(`{"targets":[
		{"priority":0,"provider":%q,"upstream_model":"m","options":{"breaker":{"enabled":true}}},
		{"priority":10,"provider":%q,"upstream_model":"mock-ok-model"}]}`, prov, okProv)
	if rec := putAlias(s, alias, body); rec.Code != http.StatusOK {
		t.Fatalf("put alias: %d %s", rec.Code, rec.Body.String())
	}
	plan, err := s.router.Resolve(ctx, alias, false)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	tripTier(t, s, plan, up)

	code, h := getAliasHealth(t, s, alias)
	if code != http.StatusOK || len(h.Tiers) != 2 {
		t.Fatalf("health: %d %+v", code, h)
	}
	t0, t1 := h.Tiers[0], h.Tiers[1]
	if t0.Tier != 0 || !t0.BreakerEnabled || t0.State != "open" || t0.Reason != "consecutive_failures" || t0.CooldownMS != 60000 {
		t.Errorf("tier 0 = %+v, want an enabled breaker open on consecutive failures with a 60s cooldown", t0)
	}
	if want := clk.Now().Add(time.Minute); !t0.OpenUntil.Equal(want) {
		t.Errorf("open_until = %v, want %v", t0.OpenUntil, want)
	}
	if len(t0.Targets) != 1 || t0.Targets[0].Provider != prov {
		t.Errorf("tier 0 targets = %+v, want the flaky provider", t0.Targets)
	}
	if t1.Tier != 10 || t1.BreakerEnabled || t1.State != "closed" {
		t.Errorf("tier 10 = %+v, want a closed tier without a breaker", t1)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/admin/aliases/"+alias+"/tiers/0/release", nil)
	req.SetPathValue("alias", alias)
	req.SetPathValue("tier", "0")
	rec := httptest.NewRecorder()
	s.handleAdminReleaseTier(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("release: %d %s", rec.Code, rec.Body.String())
	}
	if len(audited) == 0 || audited[len(audited)-1] != "alias.tier.release" {
		t.Errorf("audit actions = %v, want the release recorded", audited)
	}
	if _, h := getAliasHealth(t, s, alias); h.Tiers[0].State != "closed" {
		t.Errorf("tier 0 after release = %+v, want closed", h.Tiers[0])
	}
	up.failing.Store(false)
	if res := chat(t, s, plan); res.Provider != prov {
		t.Errorf("after release served by %q, want the released tier", res.Provider)
	}

	if code, _ := getAliasHealth(t, s, "no-such-alias-"+alias); code != http.StatusNotFound {
		t.Errorf("health of a missing alias = %d, want 404", code)
	}
}

// scrapeMetrics renders the server's metrics in the Prometheus text format.
func scrapeMetrics(t *testing.T, s *Server) string {
	t.Helper()
	rec := httptest.NewRecorder()
	s.metrics.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return rec.Body.String()
}

func TestBreakerTransitionsAreLoggedAndExported(t *testing.T) {
	logs := captureLogs(t)
	up := newSwitchableUpstream(t, true)
	clk := newFakeClock()
	s := newBreakerTestServer(t, clk, providers.NewOpenAICompat("flaky", "openai", up.URL, ""), providers.NewMock("mock-ok"))
	s.metrics.RegisterBreakerStates(s.breakerStates)
	plan := guardedPlan("voice-reply", "flaky", breakerOn())

	tripTier(t, s, plan, up)
	chat(t, s, plan) // skipped while open
	if m := scrapeMetrics(t, s); !strings.Contains(m, `airllm_breaker_state{alias="voice-reply",tier="0"} 1`) {
		t.Errorf("state gauge does not show tier 0 open:\n%s", grepLines(m, "airllm_breaker_state"))
	}
	clk.Advance(time.Minute)
	up.failing.Store(false)
	chat(t, s, plan) // the probe closes it

	for _, line := range []string{"tier breaker opened", "tier breaker probe", "tier breaker closed"} {
		if !strings.Contains(logs.String(), `"msg":"`+line+`"`) {
			t.Errorf("no %q log line in:\n%s", line, logs.String())
		}
	}
	if !strings.Contains(logs.String(), `"reason":"consecutive_failures"`) {
		t.Error("the opened line does not carry the trip reason")
	}

	m := scrapeMetrics(t, s)
	for _, want := range []string{
		`airllm_breaker_state{alias="voice-reply",tier="0"} 0`,
		`airllm_breaker_transitions_total{alias="voice-reply",tier="0",to="open"} 1`,
		`airllm_breaker_transitions_total{alias="voice-reply",tier="0",to="half_open"} 1`,
		`airllm_breaker_transitions_total{alias="voice-reply",tier="0",to="closed"} 1`,
		`airllm_tier_fallbacks_total{alias="voice-reply",from_tier="0",reason="http_503",to_tier="1"} 3`,
		`airllm_tier_fallbacks_total{alias="voice-reply",from_tier="0",reason="quarantined",to_tier="1"} 1`,
		`airllm_tier_outcomes_total{alias="voice-reply",outcome="failure",tier="0"} 3`,
		`airllm_tier_outcomes_total{alias="voice-reply",outcome="quarantined",tier="0"} 1`,
		`airllm_tier_outcomes_total{alias="voice-reply",outcome="success",tier="0"} 1`,
		`airllm_tier_outcomes_total{alias="voice-reply",outcome="success",tier="1"} 4`,
	} {
		if !strings.Contains(m, want) {
			t.Errorf("metrics lack %s\n%s", want, grepLines(m, "airllm_breaker", "airllm_tier"))
		}
	}
}

// grepLines keeps the lines of s that start with one of the prefixes.
func grepLines(s string, prefixes ...string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		for _, p := range prefixes {
			if strings.HasPrefix(l, p) {
				out = append(out, l)
				break
			}
		}
	}
	return strings.Join(out, "\n")
}
