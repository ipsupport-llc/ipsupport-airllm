package httpapi

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/audio"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/llm"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/providers"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/routing"
)

// These are the seam tests for per-tier failover policy: the time budget, the
// auth-failure flag and the attempt bookkeeping. Requests are driven through
// the executor into real providers talking to in-process upstreams, and the
// assertions are on which upstream answered, what the client received and how
// long it took. Budgets are configured in tens of milliseconds, so no test
// waits anywhere near a production timeout.

// hangingUpstream accepts every request and never answers until the caller
// gives up — the shape of a provider that is up at the TCP level but stuck.
func hangingUpstream(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		// Reading the body to the end is what lets the server notice the
		// client hanging up and cancel r.Context().
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) }) // runs first: never let Close wait on a handler
	return srv, &calls
}

func ms(n int) *int { return &n }

// budgetPlan puts the given first-tier target above a healthy mock.
func budgetPlan(first routing.Target) *routing.Plan {
	return &routing.Plan{
		Alias:    "voice-reply",
		Strategy: "round_robin",
		Tiers: [][]routing.Target{
			{first},
			{{Provider: "mock-ok", UpstreamModel: "mock-ok-model"}},
		},
	}
}

func TestStreamTierWithoutFirstChunkInBudgetFallsThrough(t *testing.T) {
	up, calls := hangingUpstream(t)
	s := newRunChatTestServer(t, providers.NewOpenAICompat("stuck", "openai", up.URL, ""), providers.NewMock("mock-ok"))
	plan := budgetPlan(routing.Target{Provider: "stuck", UpstreamModel: "m", Options: routing.TargetOptions{TimeoutMS: ms(50)}})
	req := llm.ChatRequest{Stream: true, Messages: []llm.Message{{Role: "user", Content: "hi"}}}

	var begun []string
	var content string
	sink := &recordingSink{
		onBegin: func(t routing.Target) { begun = append(begun, t.Provider) },
		onChunk: func(c llm.StreamChunk) { content += c.Content },
	}
	start := time.Now()
	res, _, started, err := s.runStream(context.Background(), plan, req, sink)
	if err != nil {
		t.Fatalf("runStream: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("took %v — the stuck tier was waited on instead of abandoned after its 50ms budget", elapsed)
	}
	if calls.Load() != 1 {
		t.Errorf("stuck upstream called %d times, want 1", calls.Load())
	}
	if !started || res.Provider != "mock-ok" {
		t.Fatalf("served by %q (started=%v), want the mock tier", res.Provider, started)
	}
	if len(begun) != 1 || begun[0] != "mock-ok" {
		t.Errorf("response begun by %v, want exactly once by mock-ok — the client must see one clean response", begun)
	}
	if content == "" {
		t.Error("no content reached the client from the fallback tier")
	}
	if res.Tier != 1 || res.Attempts != 2 {
		t.Errorf("tier=%d attempts=%d, want tier=1 attempts=2", res.Tier, res.Attempts)
	}
}

// recordingSink is a streamSink that records what the client would see.
type recordingSink struct {
	onBegin func(routing.Target)
	onChunk func(llm.StreamChunk)
}

func (r *recordingSink) begin(t routing.Target) {
	if r.onBegin != nil {
		r.onBegin(t)
	}
}

func (r *recordingSink) chunk(c llm.StreamChunk) error {
	if r.onChunk != nil {
		r.onChunk(c)
	}
	return nil
}

func TestUnaryChatTierOverBudgetFallsThrough(t *testing.T) {
	up, calls := hangingUpstream(t)
	s := newRunChatTestServer(t, providers.NewOpenAICompat("stuck", "openai", up.URL, ""), providers.NewMock("mock-ok"))
	plan := budgetPlan(routing.Target{Provider: "stuck", UpstreamModel: "m", Options: routing.TargetOptions{TimeoutMS: ms(50)}})
	req := llm.ChatRequest{Messages: []llm.Message{{Role: "user", Content: "hi"}}}

	start := time.Now()
	resp, res, err := s.runChat(context.Background(), plan, req)
	if err != nil {
		t.Fatalf("runChat: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("took %v — the stuck tier outlived its 50ms budget", elapsed)
	}
	if calls.Load() != 1 || res.Provider != "mock-ok" || len(resp.Choices) == 0 {
		t.Errorf("stuck calls=%d served=%q choices=%d, want the mock tier to answer after one stuck call", calls.Load(), res.Provider, len(resp.Choices))
	}
	if res.Tier != 1 || res.Attempts != 2 {
		t.Errorf("tier=%d attempts=%d, want tier=1 attempts=2", res.Tier, res.Attempts)
	}
}

// slowAfterFirstChunk streams one chunk at once, then takes its time over
// the rest — a healthy but slow model.
func slowAfterFirstChunk(t *testing.T, pause time.Duration) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n")
		w.(http.Flusher).Flush()
		time.Sleep(pause)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"lo\"},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestStreamBudgetBoundsOnlyTheFirstChunk(t *testing.T) {
	up := slowAfterFirstChunk(t, 200*time.Millisecond)
	s := newRunChatTestServer(t, providers.NewOpenAICompat("slow", "openai", up.URL, ""), providers.NewMock("mock-ok"))
	plan := budgetPlan(routing.Target{Provider: "slow", UpstreamModel: "m", Options: routing.TargetOptions{TimeoutMS: ms(50)}})
	req := llm.ChatRequest{Stream: true, Messages: []llm.Message{{Role: "user", Content: "hi"}}}

	var content string
	sink := &recordingSink{onChunk: func(c llm.StreamChunk) { content += c.Content }}
	res, _, _, err := s.runStream(context.Background(), plan, req, sink)
	if err != nil {
		t.Fatalf("runStream: %v", err)
	}
	if res.Provider != "slow" || content != "Hello" {
		t.Errorf("served=%q content=%q, want the slow tier's whole answer — its first chunk was in budget", res.Provider, content)
	}
}

func TestAudioTiersOverBudgetFallThrough(t *testing.T) {
	up, calls := hangingUpstream(t)
	s := newRunChatTestServer(t, providers.NewOpenAICompat("stuck", "openai", up.URL, ""), providers.NewMock("mock-ok"))
	plan := budgetPlan(routing.Target{Provider: "stuck", UpstreamModel: "m", Options: routing.TargetOptions{TimeoutMS: ms(50)}})

	start := time.Now()
	tr, res, err := s.runTranscribe(context.Background(), plan, audio.TranscriptionRequest{Audio: []byte("pcm"), Filename: "a.wav"})
	if err != nil {
		t.Fatalf("runTranscribe: %v", err)
	}
	if res.Provider != "mock-ok" || tr.Text == "" || res.Tier != 1 || res.Attempts != 2 {
		t.Errorf("transcription served=%q text=%q tier=%d attempts=%d, want the mock tier after one stuck attempt", res.Provider, tr.Text, res.Tier, res.Attempts)
	}

	sp, res, err := s.runSynthesize(context.Background(), plan, audio.SpeechRequest{Input: "hello", Voice: "v"})
	if err != nil {
		t.Fatalf("runSynthesize: %v", err)
	}
	if res.Provider != "mock-ok" || len(sp.Audio) == 0 || res.Tier != 1 || res.Attempts != 2 {
		t.Errorf("speech served=%q audio=%d bytes tier=%d attempts=%d, want the mock tier after one stuck attempt", res.Provider, len(sp.Audio), res.Tier, res.Attempts)
	}
	if calls.Load() != 2 {
		t.Errorf("stuck upstream called %d times, want once per request", calls.Load())
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("took %v — the stuck tier outlived its 50ms budget", elapsed)
	}
}
