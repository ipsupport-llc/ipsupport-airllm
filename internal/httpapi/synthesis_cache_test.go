package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/audio"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/breaker"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/metrics"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/providers"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/routing"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/speechcache"
)

// These are the seam tests for the synthesis cache. Speech requests go
// through the executor into real OpenAI-compatible providers talking to
// in-process speech upstreams, each answering with a WAV of its own sample
// rate, so the rate in a reply says which voice spoke it. The assertions are
// on what each upstream was asked and what came back. Clips expire on an
// injected clock.

// voiceUpstream is a speech route that answers every request with a WAV at
// its own rate, or a 500 while failing is set.
type voiceUpstream struct {
	*httptest.Server
	rate    int
	failing atomic.Bool
	mu      sync.Mutex
	voices  []string // the voice of every request it was asked, failed or not
}

func newVoiceUpstream(t *testing.T, rate int) *voiceUpstream {
	t.Helper()
	u := &voiceUpstream{rate: rate}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var c speechCallOpenAI
		_ = json.NewDecoder(r.Body).Decode(&c)
		u.mu.Lock()
		u.voices = append(u.voices, c.Voice)
		u.mu.Unlock()
		if u.failing.Load() {
			http.Error(w, `{"error":{"message":"unavailable"}}`, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "audio/wav")
		_, _ = w.Write(pcmWAV(u.rate, 1))
	}))
	t.Cleanup(u.Close)
	return u
}

// asked returns the voices the upstream has been asked to speak so far.
func (u *voiceUpstream) asked() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.voices...)
}

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// newCacheTestServer serves the named upstreams as OpenAI-compatible
// providers of one concurrency slot each, with the synthesis cache kept in
// process on clk.
func newCacheTestServer(t *testing.T, clk *testClock, ups map[string]*voiceUpstream) *Server {
	t.Helper()
	reg := providers.NewRegistry()
	for name, u := range ups {
		reg.Register(providers.NewOpenAICompat(name, "openai", u.URL, ""), 1)
	}
	s := &Server{router: routing.NewRouter(nil), metrics: metrics.New(), speechCache: speechcache.New(nil, speechcache.WithClock(clk.Now))}
	s.regPtr.Store(reg)
	return s
}

// cachedPlan is an alias with the synthesis cache on whose tiers 0, 1, …
// are the given targets.
func cachedPlan(targets ...routing.Target) *routing.Plan {
	p := ttsPlan(targets...)
	p.SynthesisCache, p.SynthesisCacheTTL = true, routing.DefaultSynthesisCacheTTL
	return p
}

func speak(t *testing.T, s *Server, plan *routing.Plan, input, voice string) (int, execResult) {
	t.Helper()
	out, res, err := s.runSynthesize(context.Background(), plan, audio.SpeechRequest{Input: input, Voice: voice, ResponseFormat: "wav"})
	if err != nil {
		t.Fatalf("runSynthesize(%q, %q): %v", input, voice, err)
	}
	return sampleRate(t, out.Audio), res
}

// hold takes the provider's only concurrency slot until the test releases
// it, so the executor passes the tier over as unavailable.
func hold(t *testing.T, s *Server, provider string) (release func()) {
	t.Helper()
	e, _ := s.reg().Get(provider)
	if !e.Acquire() {
		t.Fatalf("slot of %s already taken", provider)
	}
	return e.Release
}

func TestSecondIdenticalSpeechRequestIsServedFromTheCache(t *testing.T) {
	up := newVoiceUpstream(t, 22050)
	s := newCacheTestServer(t, &testClock{t: time.Now()}, map[string]*voiceUpstream{"piper": up})
	plan := cachedPlan(routing.Target{Provider: "piper", UpstreamModel: "tts-1"})

	rate, first := speak(t, s, plan, "Hello, how can I help?", "en_US-amy-medium")
	if rate != 22050 || first.Cache != cacheMiss || first.Attempts != 1 {
		t.Fatalf("first reply rate=%d cache=%q attempts=%d, want the upstream's 22050 as a miss on one attempt", rate, first.Cache, first.Attempts)
	}
	rate, second := speak(t, s, plan, "Hello, how can I help?", "en_US-amy-medium")
	if rate != 22050 || second.Cache != cacheHit {
		t.Errorf("second reply rate=%d cache=%q, want the same clip as a hit", rate, second.Cache)
	}
	if second.Provider != "piper" || second.UpstreamModel != "tts-1" || second.Attempts != 0 {
		t.Errorf("hit served by %s/%s after %d attempts, want piper/tts-1 with no upstream call", second.Provider, second.UpstreamModel, second.Attempts)
	}
	if n := len(up.asked()); n != 1 {
		t.Errorf("upstream asked %d times, want once", n)
	}
	if _, third := speak(t, s, plan, "Something else", "en_US-amy-medium"); third.Cache != cacheMiss {
		t.Errorf("different text: cache=%q, want a miss", third.Cache)
	}
}

func TestSynthesisCacheIsOffUnlessTheAliasTurnsItOn(t *testing.T) {
	up := newVoiceUpstream(t, 22050)
	s := newCacheTestServer(t, &testClock{t: time.Now()}, map[string]*voiceUpstream{"piper": up})
	plan := ttsPlan(routing.Target{Provider: "piper", UpstreamModel: "tts-1"})

	for range 2 {
		if _, res := speak(t, s, plan, "Hello", "en_US-amy-medium"); res.Cache != "" {
			t.Errorf("cache=%q on an alias without the cache, want none", res.Cache)
		}
	}
	if n := len(up.asked()); n != 2 {
		t.Errorf("upstream asked %d times, want every request to reach it", n)
	}
}

func TestBackupVoiceClipsAreCachedUnderTheBackupAndNeverServedForThePrimary(t *testing.T) {
	primary, backup := newVoiceUpstream(t, 24000), newVoiceUpstream(t, 22050)
	s := newCacheTestServer(t, &testClock{t: time.Now()}, map[string]*voiceUpstream{"google": primary, "piper": backup})
	// The same model and voice on both tiers: only the provider that spoke
	// tells their clips apart.
	plan := cachedPlan(
		routing.Target{Provider: "google", UpstreamModel: "tts-1"},
		routing.Target{Provider: "piper", UpstreamModel: "tts-1"},
	)
	primary.failing.Store(true)

	for i := range 3 {
		rate, res := speak(t, s, plan, "Thanks for calling", "en-US-Neural2-A")
		if rate != 22050 || res.Provider != "piper" {
			t.Fatalf("request %d: rate=%d served by %s, want the backup's voice during the outage", i, rate, res.Provider)
		}
		if want := map[bool]cacheOutcome{true: cacheMiss, false: cacheHit}[i == 0]; res.Cache != want {
			t.Errorf("request %d: cache=%q, want %q", i, res.Cache, want)
		}
		// Every request paid the failing primary; a hit adds no call of its own.
		if res.Attempts != 2-min(i, 1) {
			t.Errorf("request %d: attempts=%d, want %d", i, res.Attempts, 2-min(i, 1))
		}
	}
	if n := len(backup.asked()); n != 1 {
		t.Errorf("backup asked %d times, want the greeting rendered once", n)
	}

	primary.failing.Store(false)
	rate, res := speak(t, s, plan, "Thanks for calling", "en-US-Neural2-A")
	if rate != 24000 || res.Provider != "google" || res.Cache != cacheMiss {
		t.Errorf("after recovery: rate=%d served by %s cache=%q, want the primary rendering its own clip", rate, res.Provider, res.Cache)
	}
}

func TestPrimaryClipsAreHitsAgainOnceThePrimaryRecovers(t *testing.T) {
	primary, backup := newVoiceUpstream(t, 24000), newVoiceUpstream(t, 22050)
	s := newCacheTestServer(t, &testClock{t: time.Now()}, map[string]*voiceUpstream{"google": primary, "piper": backup})
	plan := cachedPlan(
		routing.Target{Provider: "google", UpstreamModel: "neural2"},
		routing.Target{Provider: "piper", UpstreamModel: "tts-1"},
	)
	speak(t, s, plan, "Goodbye", "en-US-Neural2-A")

	release := hold(t, s, "google")
	for range 2 {
		if rate, res := speak(t, s, plan, "Goodbye", "en-US-Neural2-A"); rate != 22050 || res.Provider != "piper" {
			t.Fatalf("while the primary is unavailable: rate=%d served by %s, want the backup", rate, res.Provider)
		}
	}
	release()

	rate, res := speak(t, s, plan, "Goodbye", "en-US-Neural2-A")
	if rate != 24000 || res.Provider != "google" || res.Cache != cacheHit {
		t.Errorf("after recovery: rate=%d served by %s cache=%q, want the primary's own clip as a hit", rate, res.Provider, res.Cache)
	}
	if n, m := len(primary.asked()), len(backup.asked()); n != 1 || m != 1 {
		t.Errorf("primary asked %d times and backup %d, want each to render the phrase once", n, m)
	}
}

func TestChangingAVoiceMappingMissesForThatVoiceOnly(t *testing.T) {
	up := newVoiceUpstream(t, 22050)
	s := newCacheTestServer(t, &testClock{t: time.Now()}, map[string]*voiceUpstream{"piper": up})
	mapped := func(amy string) *routing.Plan {
		return cachedPlan(routing.Target{Provider: "piper", UpstreamModel: "tts-1", Options: routing.TargetOptions{Voices: map[string]routing.VoiceChoice{
			"en-US-Neural2-F": {Voice: amy},
			"en-US-Neural2-D": {Voice: "en_US-ryan-medium"},
		}}})
	}
	before := mapped("en_US-amy-medium")
	speak(t, s, before, "Hello", "en-US-Neural2-F")
	speak(t, s, before, "Hello", "en-US-Neural2-D")

	after := mapped("en_US-lessac-medium")
	if _, res := speak(t, s, after, "Hello", "en-US-Neural2-F"); res.Cache != cacheMiss {
		t.Errorf("remapped voice: cache=%q, want a miss", res.Cache)
	}
	if _, res := speak(t, s, after, "Hello", "en-US-Neural2-D"); res.Cache != cacheHit {
		t.Errorf("unchanged voice: cache=%q, want a hit", res.Cache)
	}
	want := []string{"en_US-amy-medium", "en_US-ryan-medium", "en_US-lessac-medium"}
	if got := up.asked(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("upstream asked for %v, want %v", got, want)
	}
}

func TestCachedClipsExpireAfterTheAliasTTL(t *testing.T) {
	up := newVoiceUpstream(t, 22050)
	clk := &testClock{t: time.Now()}
	s := newCacheTestServer(t, clk, map[string]*voiceUpstream{"piper": up})
	plan := cachedPlan(routing.Target{Provider: "piper", UpstreamModel: "tts-1"})
	plan.SynthesisCacheTTL = 48 * time.Hour

	speak(t, s, plan, "Hello", "en_US-amy-medium")
	clk.Advance(47 * time.Hour)
	if _, res := speak(t, s, plan, "Hello", "en_US-amy-medium"); res.Cache != cacheHit {
		t.Errorf("within the TTL: cache=%q, want a hit", res.Cache)
	}
	clk.Advance(2 * time.Hour)
	if _, res := speak(t, s, plan, "Hello", "en_US-amy-medium"); res.Cache != cacheMiss {
		t.Errorf("past the TTL: cache=%q, want a miss", res.Cache)
	}
}

func TestOtherFormatsAndLanguagesAreCachedApart(t *testing.T) {
	up := newVoiceUpstream(t, 22050)
	s := newCacheTestServer(t, &testClock{t: time.Now()}, map[string]*voiceUpstream{"piper": up})
	plan := cachedPlan(routing.Target{Provider: "piper", UpstreamModel: "tts-1"})
	req := audio.SpeechRequest{Input: "Hola", Voice: "multi", ResponseFormat: "wav"}

	for _, r := range []audio.SpeechRequest{req, {Input: req.Input, Voice: req.Voice, ResponseFormat: "mp3"}, {Input: req.Input, Voice: req.Voice, ResponseFormat: "wav", Language: "es-ES"}} {
		if _, res, err := s.runSynthesize(context.Background(), plan, r); err != nil || res.Cache != cacheMiss {
			t.Errorf("%+v: cache=%q err=%v, want a miss of its own", r, res.Cache, err)
		}
	}
}

func TestSynthesisCacheOutcomesAreCounted(t *testing.T) {
	up := newVoiceUpstream(t, 22050)
	s := newCacheTestServer(t, &testClock{t: time.Now()}, map[string]*voiceUpstream{"piper": up})
	plan := cachedPlan(routing.Target{Provider: "piper", UpstreamModel: "tts-1"})
	for range 3 {
		speak(t, s, plan, "Hello", "en_US-amy-medium")
	}

	rec := httptest.NewRecorder()
	s.metrics.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body, _ := io.ReadAll(rec.Body)
	for _, want := range []string{
		`airllm_synthesis_cache_total{alias="voice-tts",outcome="hit",provider="piper"} 2`,
		`airllm_synthesis_cache_total{alias="voice-tts",outcome="miss",provider="piper"} 1`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("metrics missing %s", want)
		}
	}
}

func TestAnUnreadableCacheLeavesTheProviderToAnswer(t *testing.T) {
	up := newVoiceUpstream(t, 22050)
	s := newCacheTestServer(t, &testClock{t: time.Now()}, map[string]*voiceUpstream{"piper": up})
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	s.speechCache = speechcache.New(rdb)
	plan := cachedPlan(routing.Target{Provider: "piper", UpstreamModel: "tts-1"})

	for i := range 2 {
		rate, res := speak(t, s, plan, "Hello", "en_US-amy-medium")
		if rate != 22050 || res.Cache != cacheError || res.Attempts != 1 {
			t.Errorf("request %d: rate=%d cache=%q attempts=%d, want the provider's clip with outcome error", i, rate, res.Cache, res.Attempts)
		}
	}
	if n := len(up.asked()); n != 2 {
		t.Errorf("upstream asked %d times, want every request answered by it", n)
	}
}

func TestACacheHitTellsTheBreakerNothingAboutTheTier(t *testing.T) {
	primary, backup := newVoiceUpstream(t, 24000), newVoiceUpstream(t, 22050)
	clk := &testClock{t: time.Now()}
	s := newCacheTestServer(t, clk, map[string]*voiceUpstream{"google": primary, "piper": backup})
	s.breaker = s.newBreaker(nil, breaker.WithClock(clk.Now))
	plan := cachedPlan(
		routing.Target{Provider: "google", UpstreamModel: "tts-1", Options: breakerWith(routing.BreakerOptions{Failures: ms(2)})},
		routing.Target{Provider: "piper", UpstreamModel: "tts-1"},
	)
	speak(t, s, plan, "Thanks for calling", "en-US-Neural2-A")
	primary.failing.Store(true)

	speak(t, s, plan, "One moment", "en-US-Neural2-A")
	// Answered from the primary's own clip without calling it: neither a
	// success that breaks the run of failures nor a call that counts.
	if _, res := speak(t, s, plan, "Thanks for calling", "en-US-Neural2-A"); res.Provider != "google" || res.Cache != cacheHit || res.Attempts != 0 {
		t.Fatalf("served by %s cache=%q attempts=%d, want a hit on the primary with no upstream call", res.Provider, res.Cache, res.Attempts)
	}
	speak(t, s, plan, "Goodbye", "en-US-Neural2-A")

	asked := len(primary.asked())
	if _, res := speak(t, s, plan, "Still there?", "en-US-Neural2-A"); res.Provider != "piper" {
		t.Fatalf("served by %s, want the backup", res.Provider)
	}
	if n := len(primary.asked()); n != asked {
		t.Errorf("primary asked again after two failures in a row (%d calls, was %d): the hit between them reset the breaker", n, asked)
	}
}
