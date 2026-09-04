package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/anthropic"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/llm"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/providers"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/routing"
)

// These are the seam tests for the Vertex provider: requests are driven
// through routing and the real provider into an in-process upstream, and the
// assertions are on what that upstream received and what came back out. No
// cloud credentials, no network, no database — the token source is a stub and
// the upstream is an httptest server.

// stubTokens is a token source that needs no cloud. A non-nil err makes the
// exchange fail, which is how the fallback-worthiness of a token failure is
// exercised end to end.
type stubTokens struct {
	token string
	err   error
}

func (s stubTokens) Token(context.Context) (string, error) {
	if s.err != nil {
		return "", s.err
	}
	return s.token, nil
}

// recordedRequest is what the fake upstream saw.
type recordedRequest struct {
	auth string
	body map[string]any
}

// fakeVertex serves one JSON reply and records the request. status drives the
// error paths.
func fakeVertex(t *testing.T, status int, reply string) (*httptest.Server, *recordedRequest) {
	t.Helper()
	rec := &recordedRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.auth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&rec.body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, reply)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

// fakeVertexStream serves the given SSE data lines.
func fakeVertexStream(t *testing.T, lines ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, l := range lines {
			fmt.Fprintf(w, "data: %s\n\n", l)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// vertexOnlyPlan routes an alias to a single Vertex tier.
func vertexOnlyPlan() *routing.Plan {
	return &routing.Plan{
		Alias:    "chat",
		Strategy: "round_robin",
		Tiers:    [][]routing.Target{{{Provider: "vertex", UpstreamModel: "gemini-2.5-flash"}}},
	}
}

// vertexThenMockPlan puts Vertex above a healthy mock, so a failure at the
// Vertex tier is observable as the mock having served the request.
func vertexThenMockPlan() *routing.Plan {
	return &routing.Plan{
		Alias:    "chat",
		Strategy: "round_robin",
		Tiers: [][]routing.Target{
			{{Provider: "vertex", UpstreamModel: "gemini-2.5-flash"}},
			{{Provider: "mock-ok", UpstreamModel: "mock-ok-model"}},
		},
	}
}

// cumulativeUsageStream is Vertex's streaming shape: usage repeated on every
// chunk, growing as it goes, rather than once at the end.
var cumulativeUsageStream = []string{
	`{"choices":[{"delta":{"role":"assistant"}}],"usage":{"prompt_tokens":9,"completion_tokens":0,"total_tokens":9}}`,
	`{"choices":[{"delta":{"content":"Hel"}}],"usage":{"prompt_tokens":9,"completion_tokens":1,"total_tokens":10}}`,
	`{"choices":[{"delta":{"content":"lo"}}],"usage":{"prompt_tokens":9,"completion_tokens":2,"total_tokens":11}}`,
	`{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":9,"completion_tokens":4,"total_tokens":13}}`,
	"[DONE]",
}

func TestVertexChatWithToolsThroughTheChatPath(t *testing.T) {
	up, rec := fakeVertex(t, http.StatusOK, `{
		"id":"c1","model":"google/gemini-2.5-flash",
		"choices":[{"index":0,"message":{"role":"assistant","tool_calls":[
			{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"acme\"}"}}
		]},"finish_reason":"tool_calls"}],
		"usage":{"prompt_tokens":42,"completion_tokens":11,"total_tokens":53}}`)

	s := newRunChatTestServer(t, providers.NewVertex("vertex", up.URL, stubTokens{token: "ya29.stub"}))
	req := llm.ChatRequest{
		Messages: []llm.Message{{Role: "user", Content: "look up acme"}},
		Tools: []llm.Tool{{Type: "function", Function: llm.FunctionDef{
			Name: "lookup", Parameters: json.RawMessage(`{"type":"object"}`),
		}}},
	}

	resp, target, err := s.runChat(context.Background(), vertexOnlyPlan(), req)
	if err != nil {
		t.Fatalf("runChat: %v", err)
	}
	if target.Provider != "vertex" {
		t.Fatalf("target.Provider = %q, want vertex — the alias never reached the provider", target.Provider)
	}

	// What the upstream received.
	if rec.auth != "Bearer ya29.stub" {
		t.Errorf("upstream Authorization = %q, want the token source's token", rec.auth)
	}
	if got := rec.body["model"]; got != "google/gemini-2.5-flash" {
		t.Errorf("upstream model = %v, want the publisher-qualified id", got)
	}
	if _, ok := rec.body["tools"]; !ok {
		t.Error("the tool definitions did not reach the upstream")
	}

	// What came back.
	if len(resp.Choices) != 1 {
		t.Fatalf("want 1 choice, got %d", len(resp.Choices))
	}
	calls := resp.Choices[0].Message.ToolCalls
	if len(calls) != 1 || calls[0].ID != "call_1" || calls[0].Function.Name != "lookup" {
		t.Errorf("tool calls lost in the round trip: %+v", calls)
	}
	if resp.Usage.TotalTokens != 53 {
		t.Errorf("usage = %+v, want the upstream's totals so the request is metered", resp.Usage)
	}
}

func TestVertexStreamReportsUsageExactlyOnce(t *testing.T) {
	up := fakeVertexStream(t, cumulativeUsageStream...)
	s := newRunChatTestServer(t, providers.NewVertex("vertex", up.URL, stubTokens{token: "t"}))

	var chunks []llm.StreamChunk
	sink := &fakeStreamSink{onChunk: func(c llm.StreamChunk) { chunks = append(chunks, c) }}
	_, usage, started, err := s.runStream(context.Background(), vertexOnlyPlan(),
		llm.ChatRequest{Messages: []llm.Message{{Role: "user", Content: "hi"}}}, sink)
	if err != nil {
		t.Fatalf("runStream: %v", err)
	}
	if !started {
		t.Fatal("the stream never started")
	}

	n := 0
	for _, c := range chunks {
		if c.Usage != nil {
			n++
		}
	}
	if n != 1 {
		t.Errorf("client saw %d usage reports from 4 cumulative upstream reports, want exactly 1", n)
	}
	if usage.TotalTokens != 13 {
		t.Errorf("metered usage = %+v, want the final cumulative totals", usage)
	}
}

// TestVertexStreamEndsTheAnthropicMessageOnce is the reason usage is coalesced
// at all: the Anthropic egress ends the message on every usage chunk it sees,
// so a stream forwarding Vertex's cumulative reports would end early and then
// again three more times, and an SDK reading it would see a truncated answer.
func TestVertexStreamEndsTheAnthropicMessageOnce(t *testing.T) {
	up := fakeVertexStream(t, cumulativeUsageStream...)
	s := newRunChatTestServer(t, providers.NewVertex("vertex", up.URL, stubTokens{token: "t"}))

	rec := httptest.NewRecorder()
	sink := &anthropicSink{
		w:  rec,
		sw: anthropic.NewStreamWriter(rec, func() {}, "msg_test", "chat", 9),
	}
	if _, _, _, err := s.runStream(context.Background(), vertexOnlyPlan(),
		llm.ChatRequest{Messages: []llm.Message{{Role: "user", Content: "hi"}}}, sink); err != nil {
		t.Fatalf("runStream: %v", err)
	}

	out := rec.Body.String()
	if n := strings.Count(out, "event: message_stop"); n != 1 {
		t.Errorf("message_stop appeared %d times, want exactly 1:\n%s", n, out)
	}
	if n := strings.Count(out, "event: message_delta"); n != 1 {
		t.Errorf("message_delta appeared %d times, want exactly 1", n)
	}
	if got := sink.assembled(); got != "Hello" {
		t.Errorf("assembled content = %q, want the whole answer", got)
	}
}

func TestVertexModelNotFoundFallsBackToTheNextTier(t *testing.T) {
	up, _ := fakeVertex(t, http.StatusNotFound,
		`{"error":{"code":404,"message":"Publisher Model `+"`"+`google/gemini-9`+"`"+` was not found","status":"NOT_FOUND"}}`)
	s := newRunChatTestServer(t,
		providers.NewVertex("vertex", up.URL, stubTokens{token: "t"}),
		providers.NewMock("mock-ok"))

	resp, target, err := s.runChat(context.Background(), vertexThenMockPlan(),
		llm.ChatRequest{Messages: []llm.Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Vertex's not-found envelope should have fallen through, not failed the request: %v", err)
	}
	if target.Provider != "mock-ok" {
		t.Errorf("target.Provider = %q, want mock-ok", target.Provider)
	}
	if len(resp.Choices) == 0 {
		t.Error("want a real answer from the fallback tier")
	}
}

func TestVertexContextLengthExceededFallsBackToTheNextTier(t *testing.T) {
	up, _ := fakeVertex(t, http.StatusBadRequest,
		`{"error":{"code":400,"message":"The input token count (2000000) exceeds the maximum number of tokens allowed (1048576).","status":"INVALID_ARGUMENT"}}`)
	s := newRunChatTestServer(t,
		providers.NewVertex("vertex", up.URL, stubTokens{token: "t"}),
		providers.NewMock("mock-ok"))

	_, target, err := s.runChat(context.Background(), vertexThenMockPlan(),
		llm.ChatRequest{Messages: []llm.Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("an over-window prompt should reach a larger-context tier, not fail: %v", err)
	}
	if target.Provider != "mock-ok" {
		t.Errorf("target.Provider = %q, want mock-ok", target.Provider)
	}
}

func TestVertexTokenSourceFailureFallsBackToTheNextTier(t *testing.T) {
	// No upstream: a token failure must be decided before any call is made.
	s := newRunChatTestServer(t,
		providers.NewVertex("vertex", "http://127.0.0.1:1", stubTokens{err: errors.New("metadata server unreachable")}),
		providers.NewMock("mock-ok"))

	_, target, err := s.runChat(context.Background(), vertexThenMockPlan(),
		llm.ChatRequest{Messages: []llm.Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("a token-source failure should fall through, not fail the request: %v", err)
	}
	if target.Provider != "mock-ok" {
		t.Errorf("target.Provider = %q, want mock-ok", target.Provider)
	}
}
