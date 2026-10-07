package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/affinity"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/audio"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/breaker"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/llm"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/providers"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/routing"
)

// These are the seam tests for call affinity. Requests carry a session header
// into the executor, which calls real providers talking to in-process
// upstreams; the assertions are on which upstream each request of a session
// reached. Pins expire on an injected clock.

// newAffinityTestServer is newRunChatTestServer with the session pin store on
// the given clock and no Redis.
func newAffinityTestServer(t *testing.T, clk *fakeClock, ps ...providers.Provider) *Server {
	t.Helper()
	s := newRunChatTestServer(t, ps...)
	s.affinity = affinity.New(nil, affinity.WithClock(clk.Now))
	return s
}

// affinityPlan puts the given upstream providers into tiers 0, 1, … of an
// alias with session affinity on.
func affinityPlan(providerNames ...string) *routing.Plan {
	plan := &routing.Plan{Alias: "voice-reply", Strategy: "round_robin", SessionAffinity: true, SessionAffinityTTL: routing.DefaultAffinityTTL}
	for i, p := range providerNames {
		plan.Tiers = append(plan.Tiers, []routing.Target{{Provider: p, UpstreamModel: "m", Tier: i}})
	}
	return plan
}

// inSession is a request context carrying the session header, the way
// ServeHTTP builds it.
func inSession(id string) context.Context {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	if id != "" {
		r.Header.Set(clientSessionHeader, id)
	}
	return withClientSession(context.Background(), r)
}

func chatIn(t *testing.T, s *Server, ctx context.Context, plan *routing.Plan) execResult {
	t.Helper()
	_, res, err := s.runChat(ctx, plan, llm.ChatRequest{Messages: []llm.Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("runChat: %v", err)
	}
	return res
}

func TestSessionServedByABackupTierStaysThereAfterThePrimaryRecovers(t *testing.T) {
	primary, backup := newSwitchableUpstream(t, true), newSwitchableUpstream(t, false)
	s := newAffinityTestServer(t, newFakeClock(),
		providers.NewOpenAICompat("primary", "openai", primary.URL, ""),
		providers.NewOpenAICompat("backup", "openai", backup.URL, ""))
	plan := affinityPlan("primary", "backup")
	call := inSession("call-1")

	if res := chatIn(t, s, call, plan); res.Provider != "backup" {
		t.Fatalf("first request served by %q, want the backup after the primary failed", res.Provider)
	}
	primary.failing.Store(false)
	before := primary.calls.Load()
	res := chatIn(t, s, call, plan)
	if res.Provider != "backup" || res.Tier != 1 || res.Attempts != 1 {
		t.Errorf("served=%q tier=%d attempts=%d, want the backup tier on the only attempt", res.Provider, res.Tier, res.Attempts)
	}
	if primary.calls.Load() != before {
		t.Error("the recovered primary was called again — a pinned session must not move back")
	}
}

func TestPinnedSessionStillMovesForwardWhenItsTierFails(t *testing.T) {
	primary, backup, last := newSwitchableUpstream(t, true), newSwitchableUpstream(t, false), newSwitchableUpstream(t, false)
	s := newAffinityTestServer(t, newFakeClock(),
		providers.NewOpenAICompat("primary", "openai", primary.URL, ""),
		providers.NewOpenAICompat("backup", "openai", backup.URL, ""),
		providers.NewOpenAICompat("last", "openai", last.URL, ""))
	plan := affinityPlan("primary", "backup", "last")
	call := inSession("call-1")

	chatIn(t, s, call, plan) // pinned to the backup
	backup.failing.Store(true)
	primaryBefore := primary.calls.Load()
	if res := chatIn(t, s, call, plan); res.Provider != "last" || res.Attempts != 2 {
		t.Fatalf("served=%q attempts=%d, want the last tier after the pinned backup failed", res.Provider, res.Attempts)
	}
	if primary.calls.Load() != primaryBefore {
		t.Error("the primary was tried — a session pinned to tier 1 must not start earlier")
	}

	primary.failing.Store(false)
	backup.failing.Store(false)
	if res := chatIn(t, s, call, plan); res.Provider != "last" || res.Attempts != 1 {
		t.Errorf("served=%q attempts=%d, want the session to stay on the last tier once it moved there", res.Provider, res.Attempts)
	}
}

func TestANewSessionStartsAtTheFirstTier(t *testing.T) {
	primary, backup := newSwitchableUpstream(t, true), newSwitchableUpstream(t, false)
	s := newAffinityTestServer(t, newFakeClock(),
		providers.NewOpenAICompat("primary", "openai", primary.URL, ""),
		providers.NewOpenAICompat("backup", "openai", backup.URL, ""))
	plan := affinityPlan("primary", "backup")

	chatIn(t, s, inSession("call-1"), plan)
	primary.failing.Store(false)
	if res := chatIn(t, s, inSession("call-2"), plan); res.Provider != "primary" {
		t.Errorf("a new session was served by %q, want the recovered primary", res.Provider)
	}
}

func TestWithoutTheHeaderOrTheFlagRequestsGoBackToThePrimary(t *testing.T) {
	cases := map[string]struct {
		session  string
		affinity bool
	}{
		"no session header":      {session: "", affinity: true},
		"alias without the flag": {session: "call-1", affinity: false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			primary, backup := newSwitchableUpstream(t, true), newSwitchableUpstream(t, false)
			s := newAffinityTestServer(t, newFakeClock(),
				providers.NewOpenAICompat("primary", "openai", primary.URL, ""),
				providers.NewOpenAICompat("backup", "openai", backup.URL, ""))
			plan := affinityPlan("primary", "backup")
			plan.SessionAffinity = c.affinity

			if res := chatIn(t, s, inSession(c.session), plan); res.Provider != "backup" {
				t.Fatalf("first request served by %q, want the backup", res.Provider)
			}
			primary.failing.Store(false)
			if res := chatIn(t, s, inSession(c.session), plan); res.Provider != "primary" {
				t.Errorf("served by %q, want the recovered primary — nothing to pin", res.Provider)
			}
		})
	}
}

func TestPinnedSessionStaysOnTheBackupAfterTheBreakerCloses(t *testing.T) {
	primary, backup := newSwitchableUpstream(t, true), newSwitchableUpstream(t, false)
	clk := newFakeClock()
	s := newAffinityTestServer(t, clk,
		providers.NewOpenAICompat("primary", "openai", primary.URL, ""),
		providers.NewOpenAICompat("backup", "openai", backup.URL, ""))
	s.breaker = s.newBreaker(nil, breaker.WithClock(clk.Now))
	plan := affinityPlan("primary", "backup")
	plan.Tiers[0][0].Options = breakerOn()
	call := inSession("call-1")

	for i := 0; i < 3; i++ { // opens tier 0
		chatIn(t, s, inSession(fmt.Sprintf("other-%d", i)), plan)
	}
	chatIn(t, s, call, plan) // served by the backup while tier 0 is open
	clk.Advance(time.Minute)
	primary.failing.Store(false)
	if res := chatIn(t, s, inSession("prober"), plan); res.Provider != "primary" {
		t.Fatalf("probe served by %q, want the recovered primary closing its breaker", res.Provider)
	}

	before := primary.calls.Load()
	if res := chatIn(t, s, call, plan); res.Provider != "backup" {
		t.Errorf("served by %q after the breaker closed, want the backup the call was pinned to", res.Provider)
	}
	if primary.calls.Load() != before {
		t.Error("the primary was called for the pinned session")
	}
}

func TestSessionPinExpiresAfterTheAliasTTL(t *testing.T) {
	primary, backup := newSwitchableUpstream(t, true), newSwitchableUpstream(t, false)
	clk := newFakeClock()
	s := newAffinityTestServer(t, clk,
		providers.NewOpenAICompat("primary", "openai", primary.URL, ""),
		providers.NewOpenAICompat("backup", "openai", backup.URL, ""))
	plan := affinityPlan("primary", "backup")
	plan.SessionAffinityTTL = time.Hour
	call := inSession("call-1")

	chatIn(t, s, call, plan)
	primary.failing.Store(false)
	clk.Advance(59 * time.Minute)
	if res := chatIn(t, s, call, plan); res.Provider != "backup" {
		t.Fatalf("served by %q within the TTL, want the backup", res.Provider)
	}
	// That request renewed the pin: an hour after it, not after the first.
	clk.Advance(59 * time.Minute)
	if res := chatIn(t, s, call, plan); res.Provider != "backup" {
		t.Fatalf("served by %q within the renewed TTL, want the backup", res.Provider)
	}
	clk.Advance(61 * time.Minute)
	if res := chatIn(t, s, call, plan); res.Provider != "primary" {
		t.Errorf("served by %q after the TTL, want the session to start over at the primary", res.Provider)
	}
}

func TestPinToATierTheAliasNoLongerHasIsIgnored(t *testing.T) {
	primary, backup := newSwitchableUpstream(t, true), newSwitchableUpstream(t, false)
	s := newAffinityTestServer(t, newFakeClock(),
		providers.NewOpenAICompat("primary", "openai", primary.URL, ""),
		providers.NewOpenAICompat("backup", "openai", backup.URL, ""))
	plan := affinityPlan("primary", "backup")
	call := inSession("call-1")

	chatIn(t, s, call, plan)
	primary.failing.Store(false)
	plan.Tiers = plan.Tiers[:1] // the backup tier was removed from the alias
	if res := chatIn(t, s, call, plan); res.Provider != "primary" {
		t.Errorf("served by %q, want the primary — the pinned tier is gone", res.Provider)
	}
}

func TestSessionWhosePinnedTierWasRemovedPinsAgain(t *testing.T) {
	primary, backup, last := newSwitchableUpstream(t, true), newSwitchableUpstream(t, true), newSwitchableUpstream(t, false)
	s := newAffinityTestServer(t, newFakeClock(),
		providers.NewOpenAICompat("primary", "openai", primary.URL, ""),
		providers.NewOpenAICompat("backup", "openai", backup.URL, ""),
		providers.NewOpenAICompat("last", "openai", last.URL, ""))
	full := affinityPlan("primary", "backup", "last")
	call := inSession("call-1")

	chatIn(t, s, call, full) // pinned to the last tier
	backup.failing.Store(false)
	shrunk := affinityPlan("primary", "backup") // the last tier was removed
	if res := chatIn(t, s, call, shrunk); res.Provider != "backup" {
		t.Fatalf("served by %q, want the backup once the pinned tier is gone", res.Provider)
	}
	primary.failing.Store(false)
	if res := chatIn(t, s, call, full); res.Provider != "backup" {
		t.Errorf("served by %q, want the backup the session moved to — not the primary, nor the old pin's tier", res.Provider)
	}
}

func TestAffinityAppliesToStreamsTranscriptionAndSpeech(t *testing.T) {
	primary := newSwitchableUpstream(t, true)
	s := newAffinityTestServer(t, newFakeClock(),
		providers.NewOpenAICompat("primary", "openai", primary.URL, ""),
		providers.NewMock("backup"))
	plan := affinityPlan("primary", "backup")

	requests := map[string]func(ctx context.Context) (execResult, error){
		"stream": func(ctx context.Context) (execResult, error) {
			res, _, _, err := s.runStream(ctx, plan, llm.ChatRequest{Stream: true, Messages: []llm.Message{{Role: "user", Content: "hi"}}}, &recordingSink{})
			return res, err
		},
		"transcription": func(ctx context.Context) (execResult, error) {
			_, res, err := s.runTranscribe(ctx, plan, audio.TranscriptionRequest{Audio: []byte("pcm"), Filename: "a.wav"})
			return res, err
		},
		"speech": func(ctx context.Context) (execResult, error) {
			_, res, err := s.runSynthesize(ctx, plan, audio.SpeechRequest{Input: "hello", Voice: "v"})
			return res, err
		},
	}
	for name, do := range requests {
		t.Run(name, func(t *testing.T) {
			primary.failing.Store(true)
			call := inSession("call-" + name)
			if res, err := do(call); err != nil || res.Provider != "backup" {
				t.Fatalf("first request: served=%q err=%v, want the backup", res.Provider, err)
			}
			primary.failing.Store(false)
			before := primary.calls.Load()
			if res, err := do(call); err != nil || res.Provider != "backup" || res.Session != "call-"+name {
				t.Errorf("served=%q session=%q err=%v, want the pinned backup and the session recorded", res.Provider, res.Session, err)
			}
			if primary.calls.Load() != before {
				t.Error("the primary was called for the pinned session")
			}
		})
	}
}

func TestSessionPinsAreSharedThroughRedis(t *testing.T) {
	rdb := testBreakerRedis(t)
	primary, backup := newSwitchableUpstream(t, true), newSwitchableUpstream(t, false)
	newReplica := func() *Server {
		s := newRunChatTestServer(t,
			providers.NewOpenAICompat("primary", "openai", primary.URL, ""),
			providers.NewOpenAICompat("backup", "openai", backup.URL, ""))
		s.affinity = affinity.New(rdb)
		return s
	}
	a, b := newReplica(), newReplica()
	plan := affinityPlan("primary", "backup")
	plan.Alias = fmt.Sprintf("shared-%d", time.Now().UnixNano())
	plan.SessionAffinityTTL = time.Minute
	call := inSession("call-1")

	chatIn(t, a, call, plan)
	primary.failing.Store(false)
	if res := chatIn(t, b, call, plan); res.Provider != "backup" {
		t.Errorf("replica b served the session by %q, want the backup replica a pinned it to", res.Provider)
	}
}

func TestSessionPinsSurviveARedisOutage(t *testing.T) {
	// Nothing listens on this address: every Redis call fails at once.
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 50 * time.Millisecond, MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	primary, backup := newSwitchableUpstream(t, true), newSwitchableUpstream(t, false)
	s := newRunChatTestServer(t,
		providers.NewOpenAICompat("primary", "openai", primary.URL, ""),
		providers.NewOpenAICompat("backup", "openai", backup.URL, ""))
	s.affinity = affinity.New(rdb)
	plan := affinityPlan("primary", "backup")
	call := inSession("call-1")

	chatIn(t, s, call, plan)
	primary.failing.Store(false)
	start := time.Now()
	if res := chatIn(t, s, call, plan); res.Provider != "backup" {
		t.Errorf("served by %q with Redis down, want the backup this replica pinned", res.Provider)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("a request with Redis down took %v", elapsed)
	}
}
