package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/audio"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/llm"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/metrics"
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
			{{Provider: "mock-ok", UpstreamModel: "mock-ok-model", Tier: 1}},
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

// replyingUpstream answers every request with one fixed status and body.
func replyingUpstream(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func boolp(b bool) *bool { return &b }

// authRejections are upstream replies that mean "the gateway's own
// credentials or account were refused", not "the request is bad".
var authRejections = []struct {
	name   string
	status int
	body   string
}{
	{"401 invalid key", 401, `{"error":{"message":"Incorrect API key provided","type":"invalid_request_error","code":"invalid_api_key"}}`},
	{"403 forbidden", 403, `{"error":{"message":"forbidden"}}`},
	{"google permission denied", 403, `{"error":{"code":403,"message":"Permission denied on resource","status":"PERMISSION_DENIED"}}`},
	{"google billing disabled", 403, `{"error":{"code":403,"message":"This API method requires billing to be enabled","status":"PERMISSION_DENIED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"BILLING_DISABLED"}]}}`},
	{"google failed precondition", 400, `{"error":{"code":400,"message":"Project is not allowed to use this service","status":"FAILED_PRECONDITION"}}`},
}

func TestAuthFailureFallsThroughWhenTheTargetSaysSo(t *testing.T) {
	for _, c := range authRejections {
		t.Run(c.name, func(t *testing.T) {
			up := replyingUpstream(t, c.status, c.body)
			s := newRunChatTestServer(t, providers.NewOpenAICompat("denied", "openai", up.URL, "sk"), providers.NewMock("mock-ok"))
			plan := budgetPlan(routing.Target{Provider: "denied", UpstreamModel: "m", Options: routing.TargetOptions{FallbackOnAuth: boolp(true)}})
			req := llm.ChatRequest{Messages: []llm.Message{{Role: "user", Content: "hi"}}}

			_, res, err := s.runChat(context.Background(), plan, req)
			if err != nil || res.Provider != "mock-ok" {
				t.Fatalf("served=%q err=%v, want the next tier to answer", res.Provider, err)
			}
		})
	}
}

func TestAuthFailureStopsTheRequestByDefault(t *testing.T) {
	for _, c := range authRejections {
		t.Run(c.name, func(t *testing.T) {
			up := replyingUpstream(t, c.status, c.body)
			s := newRunChatTestServer(t, providers.NewOpenAICompat("denied", "openai", up.URL, "sk"), providers.NewMock("mock-ok"))
			plan := budgetPlan(routing.Target{Provider: "denied", UpstreamModel: "m"})
			req := llm.ChatRequest{Messages: []llm.Message{{Role: "user", Content: "hi"}}}

			_, res, err := s.runChat(context.Background(), plan, req)
			if err == nil || res.Provider != "denied" || res.Attempts != 1 {
				t.Fatalf("served=%q attempts=%d err=%v, want the request to fail at the first tier as before", res.Provider, res.Attempts, err)
			}
		})
	}
}

func TestAuthFallbackDefaultComesFromSettings(t *testing.T) {
	c := authRejections[0]
	up := replyingUpstream(t, c.status, c.body)
	s := newRunChatTestServer(t, providers.NewOpenAICompat("denied", "openai", up.URL, "sk"), providers.NewMock("mock-ok"))
	s.failoverPtr.Store(&failoverConfig{FallbackOnAuth: true})
	req := llm.ChatRequest{Messages: []llm.Message{{Role: "user", Content: "hi"}}}

	_, res, err := s.runChat(context.Background(), budgetPlan(routing.Target{Provider: "denied", UpstreamModel: "m"}), req)
	if err != nil || res.Provider != "mock-ok" {
		t.Errorf("unset target option: served=%q err=%v, want the gateway default to fall through", res.Provider, err)
	}

	optOut := routing.Target{Provider: "denied", UpstreamModel: "m", Options: routing.TargetOptions{FallbackOnAuth: boolp(false)}}
	_, res, err = s.runChat(context.Background(), budgetPlan(optOut), req)
	if err == nil || res.Provider != "denied" {
		t.Errorf("target opted out: served=%q err=%v, want the target's own false to win over the default", res.Provider, err)
	}
}

func TestTimeoutDefaultComesFromSettings(t *testing.T) {
	up, _ := hangingUpstream(t)
	s := newRunChatTestServer(t, providers.NewOpenAICompat("stuck", "openai", up.URL, ""), providers.NewMock("mock-ok"))
	s.failoverPtr.Store(&failoverConfig{TimeoutMS: 50})
	req := llm.ChatRequest{Messages: []llm.Message{{Role: "user", Content: "hi"}}}

	_, res, err := s.runChat(context.Background(), budgetPlan(routing.Target{Provider: "stuck", UpstreamModel: "m"}), req)
	if err != nil || res.Provider != "mock-ok" {
		t.Errorf("served=%q err=%v, want the gateway-wide budget to abandon the stuck tier", res.Provider, err)
	}
}

func TestOllamaModelNotFoundFallsThrough(t *testing.T) {
	up := replyingUpstream(t, 404, `{"error":{"message":"model \"gemma3:27b\" not found, try pulling it first","type":"api_error","param":null,"code":null}}`)
	s := newRunChatTestServer(t, providers.NewOpenAICompat("ollama", "ollama", up.URL, ""), providers.NewMock("mock-ok"))
	req := llm.ChatRequest{Messages: []llm.Message{{Role: "user", Content: "hi"}}}

	_, res, err := s.runChat(context.Background(), budgetPlan(routing.Target{Provider: "ollama", UpstreamModel: "gemma3:27b"}), req)
	if err != nil || res.Provider != "mock-ok" {
		t.Fatalf("served=%q err=%v, want the next tier to answer", res.Provider, err)
	}

	only := &routing.Plan{Alias: "local", Tiers: [][]routing.Target{{{Provider: "ollama", UpstreamModel: "gemma3:27b"}}}}
	_, _, err = s.runChat(context.Background(), only, req)
	if code, typ := classifyUpstreamErr(err); code != http.StatusBadRequest || typ != "invalid_request_error" {
		t.Errorf("exhausted on a missing model: got (%d, %q), want (400, invalid_request_error)", code, typ)
	}
}

// countingUpstream answers every chat request with a minimal completion and
// counts how often it was asked.
func countingUpstream(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"c","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func TestClientDisconnectIsNotATierFailure(t *testing.T) {
	stuck, _ := hangingUpstream(t)
	backup, backupCalls := countingUpstream(t)
	s := newRunChatTestServer(t,
		providers.NewOpenAICompat("stuck", "openai", stuck.URL, ""),
		providers.NewOpenAICompat("backup", "openai", backup.URL, ""))
	plan := &routing.Plan{Alias: "voice-reply", Tiers: [][]routing.Target{
		{{Provider: "stuck", UpstreamModel: "m", Options: routing.TargetOptions{TimeoutMS: ms(5000)}}},
		{{Provider: "backup", UpstreamModel: "m"}},
	}}
	logs := captureLogs(t)

	for _, stream := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(30*time.Millisecond, cancel)
		req := llm.ChatRequest{Stream: stream, Messages: []llm.Message{{Role: "user", Content: "hi"}}}
		var err error
		if stream {
			_, _, _, err = s.runStream(ctx, plan, req, &recordingSink{})
		} else {
			_, _, err = s.runChat(ctx, plan, req)
		}
		if err == nil {
			t.Errorf("stream=%v: want an error once the client went away", stream)
		}
	}
	if backupCalls.Load() != 0 {
		t.Errorf("backup tier called %d times for requests nobody was waiting for", backupCalls.Load())
	}
	if strings.Contains(logs.String(), "tier attempt failed") {
		t.Errorf("a client disconnect was logged as a tier failure:\n%s", logs)
	}
}

func TestFailedAttemptIsLoggedWithItsContext(t *testing.T) {
	up := replyingUpstream(t, 503, `{"error":{"message":"overloaded"}}`)
	s := newRunChatTestServer(t, providers.NewOpenAICompat("flaky", "openai", up.URL, ""), providers.NewMock("mock-ok"))
	logs := captureLogs(t)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set("X-Session-Id", "call-42")
	ctx := withClientSession(context.Background(), r)
	req := llm.ChatRequest{Messages: []llm.Message{{Role: "user", Content: "hi"}}}

	if _, _, err := s.runChat(ctx, budgetPlan(routing.Target{Provider: "flaky", UpstreamModel: "big-model"}), req); err != nil {
		t.Fatalf("runChat: %v", err)
	}
	var line map[string]any
	for _, l := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var rec map[string]any
		if json.Unmarshal([]byte(l), &rec) == nil && rec["msg"] == "tier attempt failed" {
			line = rec
		}
	}
	if line == nil {
		t.Fatalf("no failed-attempt line logged:\n%s", logs)
	}
	want := map[string]any{"alias": "voice-reply", "tier": float64(0), "provider": "flaky", "upstream_model": "big-model", "reason": "http_503", "session": "call-42"}
	for k, v := range want {
		if line[k] != v {
			t.Errorf("%s = %v, want %v", k, line[k], v)
		}
	}
	if _, ok := line["latency_ms"]; !ok {
		t.Error("latency_ms missing")
	}
}

// captureLogs routes the default logger into a JSON buffer for the test.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// chatOnly is a provider with no audio capability.
type chatOnly struct{ name string }

func (c chatOnly) Name() string     { return c.name }
func (c chatOnly) Kind() string     { return "openai" }
func (c chatOnly) Protocol() string { return "openai" }
func (c chatOnly) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}
func (c chatOnly) ChatStream(context.Context, llm.ChatRequest, func(llm.StreamChunk) error) error {
	return nil
}

func TestAudioCapableButBusyTargetIsReportedAsBusy(t *testing.T) {
	reg := providers.NewRegistry()
	reg.Register(chatOnly{name: "text-only"}, 0)
	reg.Register(providers.NewMock("speaker"), 1)
	s := &Server{router: routing.NewRouter(nil), metrics: metrics.New()}
	s.regPtr.Store(reg)
	busy, _ := reg.Get("speaker")
	if !busy.Acquire() {
		t.Fatal("could not occupy the speaker's only slot")
	}
	defer busy.Release()
	plan := &routing.Plan{Alias: "tts", Tiers: [][]routing.Target{
		{{Provider: "text-only", UpstreamModel: "m"}},
		{{Provider: "speaker", UpstreamModel: "tts-1"}},
	}}

	_, _, err := s.runSynthesize(context.Background(), plan, audio.SpeechRequest{Input: "hi"})
	if code, _ := classifyUpstreamErr(err); code != http.StatusTooManyRequests {
		t.Errorf("got %v (status %d), want 429: a capable target was only busy", err, code)
	}
}

func TestSessionHeaderReachesTheHandler(t *testing.T) {
	s := &Server{mux: http.NewServeMux(), metrics: metrics.New()}
	var seen string
	s.mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		seen = clientSessionFrom(r.Context())
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}"))
	r.Header.Set("X-Session-Id", "call-7")
	s.ServeHTTP(httptest.NewRecorder(), r)
	if seen != "call-7" {
		t.Errorf("session in handler = %q, want call-7", seen)
	}
}
