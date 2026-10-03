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
	"testing"
	"time"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/llm"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/providers"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/routing"
)

// These are the seam tests for a tier's `thinking: "off"` option and for the
// streamed time budget waiting on real content. Requests go through the
// executor into real provider adapters talking to in-process upstreams; the
// assertions are on the body the upstream received and on what the client
// was sent.

func thinkingOff() routing.TargetOptions {
	off := "off"
	return routing.TargetOptions{Thinking: &off}
}

// bodyRecordingUpstream answers every chat request with one short streamed
// or unary reply and keeps the last request body it received.
type bodyRecordingUpstream struct {
	*httptest.Server
	mu   sync.Mutex
	body map[string]any
}

func newBodyRecordingUpstream(t *testing.T) *bodyRecordingUpstream {
	t.Helper()
	u := &bodyRecordingUpstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		u.mu.Lock()
		u.body = body
		u.mu.Unlock()
		if stream, _ := body["stream"].(bool); stream {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Paris\"},\"finish_reason\":\"stop\"}]}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"Paris"},"finish_reason":"stop"}]}`)
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *bodyRecordingUpstream) received() map[string]any {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.body
}

type staticToken struct{}

func (staticToken) Token(context.Context) (string, error) { return "t", nil }

// clientAsksToThink is a request that carries the client's own reasoning
// settings, which a thinking-off tier must not forward.
func clientAsksToThink(stream bool) llm.ChatRequest {
	return llm.ChatRequest{
		Stream:   stream,
		Messages: []llm.Message{{Role: "user", Content: "capital of France?"}},
		Extra: map[string]json.RawMessage{
			"reasoning_effort": json.RawMessage(`"high"`),
			"reasoning":        json.RawMessage(`{"effort":"high"}`),
			"think":            json.RawMessage(`true`),
		},
	}
}

func onePlan(t routing.Target) *routing.Plan {
	return &routing.Plan{Alias: "voice-reply", Strategy: "round_robin", Tiers: [][]routing.Target{{t}}}
}

func TestThinkingOffSendsEachProviderKindItsOwnSetting(t *testing.T) {
	cases := []struct {
		kind string
		want map[string]any // the reasoning fields the upstream must receive, and nothing else
	}{
		{"ollama", map[string]any{"reasoning_effort": "none"}},
		{"vertex", map[string]any{"reasoning_effort": "minimal"}},
		{"openrouter", map[string]any{"reasoning": map[string]any{"effort": "none"}}},
		{"xai", map[string]any{"reasoning_effort": "minimal"}},
		{"groq", map[string]any{}}, // no confirmed setting: only the client's is dropped
	}
	for _, c := range cases {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", c.kind, stream), func(t *testing.T) {
				up := newBodyRecordingUpstream(t)
				var p providers.Provider = providers.NewOpenAICompat("tier", c.kind, up.URL, "")
				if c.kind == "vertex" {
					p = providers.NewVertex("tier", up.URL, staticToken{})
				}
				s := newRunChatTestServer(t, p)
				plan := onePlan(routing.Target{Provider: "tier", UpstreamModel: "m", Options: thinkingOff()})

				if stream {
					if _, _, _, err := s.runStream(context.Background(), plan, clientAsksToThink(true), &recordingSink{}); err != nil {
						t.Fatalf("runStream: %v", err)
					}
				} else if _, _, err := s.runChat(context.Background(), plan, clientAsksToThink(false)); err != nil {
					t.Fatalf("runChat: %v", err)
				}

				got := map[string]any{}
				for _, k := range []string{"reasoning_effort", "reasoning", "think"} {
					if v, ok := up.received()[k]; ok {
						got[k] = v
					}
				}
				if fmt.Sprint(got) != fmt.Sprint(c.want) {
					t.Errorf("upstream got reasoning fields %v, want %v", got, c.want)
				}
			})
		}
	}
}

func TestClientReasoningPassesThroughWithoutTheOption(t *testing.T) {
	up := newBodyRecordingUpstream(t)
	s := newRunChatTestServer(t, providers.NewOpenAICompat("tier", "ollama", up.URL, ""))
	plan := onePlan(routing.Target{Provider: "tier", UpstreamModel: "m"})

	if _, _, err := s.runChat(context.Background(), plan, clientAsksToThink(false)); err != nil {
		t.Fatalf("runChat: %v", err)
	}
	if got := up.received()["reasoning_effort"]; got != "high" {
		t.Errorf("reasoning_effort = %v, want the client's own \"high\" untouched", got)
	}
}

// sseUpstream streams the given data payloads as they are, then [DONE].
func sseUpstream(t *testing.T, payloads ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, p := range payloads {
			fmt.Fprintf(w, "data: %s\n\n", p)
			w.(http.Flusher).Flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv
}

func contentDelta(s string) string {
	b, _ := json.Marshal(s)
	return fmt.Sprintf(`{"choices":[{"delta":{"content":%s}}]}`, b)
}

func TestThinkingOffStripsThinkBlocksSplitAcrossChunks(t *testing.T) {
	up := sseUpstream(t,
		contentDelta("<thi"), contentDelta("nk>the user wants a capital</th"),
		contentDelta("ink>\nPar"), contentDelta("is <b>is</b> it."),
	)
	s := newRunChatTestServer(t, providers.NewOpenAICompat("tier", "ollama", up.URL, ""))
	plan := onePlan(routing.Target{Provider: "tier", UpstreamModel: "m", Options: thinkingOff()})

	var content string
	sink := &recordingSink{onChunk: func(c llm.StreamChunk) { content += c.Content }}
	if _, _, _, err := s.runStream(context.Background(), plan, llm.ChatRequest{Stream: true}, sink); err != nil {
		t.Fatalf("runStream: %v", err)
	}
	if content != "Paris <b>is</b> it." {
		t.Errorf("client got %q, want the reply without the thinking block", content)
	}
}

func TestThinkingOffStripsThinkBlocksFromAUnaryReply(t *testing.T) {
	up := replyingUpstream(t, 200, `{"choices":[{"index":0,"message":{"role":"assistant","content":"<think>hmm</think>\n\nParis."},"finish_reason":"stop"}]}`)
	s := newRunChatTestServer(t, providers.NewOpenAICompat("tier", "ollama", up.URL, ""))
	plan := onePlan(routing.Target{Provider: "tier", UpstreamModel: "m", Options: thinkingOff()})

	resp, _, err := s.runChat(context.Background(), plan, llm.ChatRequest{})
	if err != nil {
		t.Fatalf("runChat: %v", err)
	}
	if got := resp.Choices[0].Message.Content; got != "Paris." {
		t.Errorf("content = %q, want the reply without the thinking block", got)
	}
}

func TestThinkBlocksAreLeftAloneWithoutTheOption(t *testing.T) {
	up := replyingUpstream(t, 200, `{"choices":[{"index":0,"message":{"role":"assistant","content":"<think>hmm</think>Paris."},"finish_reason":"stop"}]}`)
	s := newRunChatTestServer(t, providers.NewOpenAICompat("tier", "ollama", up.URL, ""))

	resp, _, err := s.runChat(context.Background(), onePlan(routing.Target{Provider: "tier", UpstreamModel: "m"}), llm.ChatRequest{})
	if err != nil {
		t.Fatalf("runChat: %v", err)
	}
	if got := resp.Choices[0].Message.Content; got != "<think>hmm</think>Paris." {
		t.Errorf("content = %q, want it verbatim", got)
	}
}

func TestThinkingOffSettingRejectedByTheUpstreamFallsThrough(t *testing.T) {
	rejection := `{"code":"invalid-argument","error":"This model does not support ` + "`reasoning_effort`" + ` value ` + "`minimal`" + `."}`
	up := replyingUpstream(t, 400, rejection)
	s := newRunChatTestServer(t, providers.NewOpenAICompat("tier", "xai", up.URL, ""), providers.NewMock("mock-ok"))

	_, res, err := s.runChat(context.Background(), budgetPlan(routing.Target{Provider: "tier", UpstreamModel: "m", Options: thinkingOff()}), llm.ChatRequest{})
	if err != nil || res.Provider != "mock-ok" {
		t.Fatalf("served=%q err=%v: the tier refused the setting the gateway chose for it, so the next tier must answer", res.Provider, err)
	}

	// Without the option the same refusal is the client's own doing.
	_, _, err = s.runChat(context.Background(), budgetPlan(routing.Target{Provider: "tier", UpstreamModel: "m"}), llm.ChatRequest{})
	if err == nil {
		t.Error("a client-chosen reasoning setting the upstream refuses must fail the request, not be retried elsewhere")
	}
}

// thinkingThenSilentUpstream starts a stream at once with a role chunk and
// empty deltas — what a model sends while it thinks — and then sends nothing
// else until the caller gives up.
func thinkingThenSilentUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n")
		for i := 0; i < 3; i++ {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{}}]}\n\n")
		}
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	return srv
}

func TestStreamBudgetWaitsForContentNotEmptyChunks(t *testing.T) {
	up := thinkingThenSilentUpstream(t)
	s := newRunChatTestServer(t, providers.NewOpenAICompat("thinker", "openai", up.URL, ""), providers.NewMock("mock-ok"))
	plan := budgetPlan(routing.Target{Provider: "thinker", UpstreamModel: "m", Options: routing.TargetOptions{TimeoutMS: ms(50)}})

	var begun []string
	var content string
	sink := &recordingSink{
		onBegin: func(t routing.Target) { begun = append(begun, t.Provider) },
		onChunk: func(c llm.StreamChunk) { content += c.Content },
	}
	// Bounded, so a regression fails here instead of hanging on the silent tier.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	res, _, _, err := s.runStream(ctx, plan, llm.ChatRequest{Stream: true, Messages: []llm.Message{{Role: "user", Content: "hi"}}}, sink)
	if err != nil {
		t.Fatalf("runStream: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("took %v — empty chunks kept the silent tier alive past its 50ms budget", elapsed)
	}
	if res.Provider != "mock-ok" || strings.Join(begun, ",") != "mock-ok" || content == "" {
		t.Errorf("served=%q begun=%v content=%q, want one clean answer from the next tier", res.Provider, begun, content)
	}
}

func TestStreamBudgetHoldsEmptyChunksUntilContentArrives(t *testing.T) {
	up := sseUpstream(t, `{"choices":[{"delta":{"role":"assistant"}}]}`, `{"choices":[{"delta":{}}]}`, contentDelta("Paris"))
	s := newRunChatTestServer(t, providers.NewOpenAICompat("tier", "openai", up.URL, ""), providers.NewMock("mock-ok"))
	plan := budgetPlan(routing.Target{Provider: "tier", UpstreamModel: "m", Options: routing.TargetOptions{TimeoutMS: ms(1000)}})

	var chunks []llm.StreamChunk
	sink := &recordingSink{onChunk: func(c llm.StreamChunk) { chunks = append(chunks, c) }}
	res, _, _, err := s.runStream(context.Background(), plan, llm.ChatRequest{Stream: true}, sink)
	if err != nil || res.Provider != "tier" {
		t.Fatalf("served=%q err=%v, want the first tier", res.Provider, err)
	}
	if len(chunks) == 0 || chunks[0].Role != "assistant" {
		t.Errorf("chunks %+v: the held role chunk must still reach the client first", chunks)
	}
	var content string
	for _, c := range chunks {
		content += c.Content
	}
	if content != "Paris" {
		t.Errorf("content = %q, want \"Paris\"", content)
	}
}
