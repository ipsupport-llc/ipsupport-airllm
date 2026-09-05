package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/llm"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/metrics"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/providers"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/routing"
)

func TestWriteSSEHeadersBackendModel(t *testing.T) {
	cases := []struct {
		name          string
		exposeBackend bool
		label         string
		wantHeader    string
	}{
		{"exposed with label", true, "Fast Tier", "Fast Tier"},
		{"exposed with no label configured", true, "", ""},
		{"not exposed even with a label", false, "Fast Tier", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeSSEHeaders(rec, routing.Target{Provider: "real-provider", UpstreamModel: "real-model", DisplayLabel: c.label}, c.exposeBackend)
			got := rec.Header().Get("X-Backend-Model")
			if got != c.wantHeader {
				t.Errorf("X-Backend-Model = %q, want %q", got, c.wantHeader)
			}
			if rec.Header().Get("X-Backend-Provider") != "" {
				t.Errorf("X-Backend-Provider must never be set (real provider names stay internal), got %q", rec.Header().Get("X-Backend-Provider"))
			}
		})
	}
}

// newRunChatTestServer builds a bare *Server wired only with what
// runChat/runStream touch: the provider registry and the router. No DB,
// no Redis, no HTTP — mirrors dlp_guardrails_test.go's narrow *Server
// construction pattern for the same reason (these are pure-logic tests).
func newRunChatTestServer(t *testing.T, providersToRegister ...providers.Provider) *Server {
	t.Helper()
	reg := providers.NewRegistry()
	for _, p := range providersToRegister {
		reg.Register(p, 0)
	}
	s := &Server{router: routing.NewRouter(nil), metrics: metrics.New()}
	s.regPtr.Store(reg)
	return s
}

func twoTierPlan() *routing.Plan {
	return &routing.Plan{
		Alias:    "text",
		Strategy: "round_robin",
		Tiers: [][]routing.Target{
			{{Provider: "mock-ctxfail", UpstreamModel: "mock-ctxfail-model"}},
			{{Provider: "mock-ok", UpstreamModel: "mock-ok-model"}},
		},
	}
}

func TestRunChatFallsBackOnContextLengthExceeded(t *testing.T) {
	s := newRunChatTestServer(t, providers.NewMock("mock-ctxfail"), providers.NewMock("mock-ok"))
	req := llm.ChatRequest{Messages: []llm.Message{{Role: "user", Content: "hi"}}}

	resp, target, err := s.runChat(context.Background(), twoTierPlan(), req)
	if err != nil {
		t.Fatalf("expected fallback to the second tier to succeed, got error: %v", err)
	}
	if target.Provider != "mock-ok" {
		t.Errorf("target.Provider = %q, want mock-ok (fallback should have been tried)", target.Provider)
	}
	if len(resp.Choices) == 0 {
		t.Error("expected a real response from the fallback tier")
	}
}

func TestRunStreamFallsBackOnContextLengthExceeded(t *testing.T) {
	s := newRunChatTestServer(t, providers.NewMock("mock-ctxfail"), providers.NewMock("mock-ok"))
	req := llm.ChatRequest{Messages: []llm.Message{{Role: "user", Content: "hi"}}}

	var chunks []llm.StreamChunk
	sink := &fakeStreamSink{onChunk: func(c llm.StreamChunk) { chunks = append(chunks, c) }}
	target, _, started, err := s.runStream(context.Background(), twoTierPlan(), req, sink)
	if err != nil {
		t.Fatalf("expected fallback to the second tier to succeed, got error: %v", err)
	}
	if !started {
		t.Error("expected the stream to have started")
	}
	if !sink.began {
		t.Error("expected sink.begin to have been called by the fallback tier")
	}
	if target.Provider != "mock-ok" {
		t.Errorf("target.Provider = %q, want mock-ok (fallback should have been tried)", target.Provider)
	}
	if len(chunks) == 0 {
		t.Error("expected at least one chunk from the fallback tier")
	}
}

// fakeStreamSink is a minimal streamSink for exec_test.go — records chunks,
// never writes real HTTP output.
type fakeStreamSink struct {
	onChunk func(llm.StreamChunk)
	began   bool
}

func (f *fakeStreamSink) begin(routing.Target) { f.began = true }
func (f *fakeStreamSink) chunk(c llm.StreamChunk) error {
	f.onChunk(c)
	return nil
}

func exhaustedTwoTierPlan() *routing.Plan {
	return &routing.Plan{
		Alias:    "text",
		Strategy: "round_robin",
		Tiers: [][]routing.Target{
			{{Provider: "mock-ctxfail-a", UpstreamModel: "model-ctxfail-a"}},
			{{Provider: "mock-ctxfail-b", UpstreamModel: "model-ctxfail-b"}},
		},
	}
}

func TestRunChatExhaustionReturnsLastAttemptedTarget(t *testing.T) {
	s := newRunChatTestServer(t, providers.NewMock("mock-ctxfail-a"), providers.NewMock("mock-ctxfail-b"))
	req := llm.ChatRequest{Messages: []llm.Message{{Role: "user", Content: "hi"}}}

	_, target, err := s.runChat(context.Background(), exhaustedTwoTierPlan(), req)
	if err == nil {
		t.Fatal("expected an error when every tier fails")
	}
	if target.Provider != "mock-ctxfail-b" {
		t.Errorf("target.Provider = %q, want mock-ctxfail-b (the last tier attempted, not an empty target)", target.Provider)
	}
	code, typ := classifyUpstreamErr(err)
	if code != http.StatusBadRequest || typ != "invalid_request_error" {
		t.Errorf("got (%d, %q), want (400, invalid_request_error)", code, typ)
	}
}

func TestRunStreamExhaustionReturnsLastAttemptedTarget(t *testing.T) {
	s := newRunChatTestServer(t, providers.NewMock("mock-ctxfail-a"), providers.NewMock("mock-ctxfail-b"))
	req := llm.ChatRequest{Messages: []llm.Message{{Role: "user", Content: "hi"}}}
	sink := &fakeStreamSink{onChunk: func(llm.StreamChunk) {}}

	target, _, started, err := s.runStream(context.Background(), exhaustedTwoTierPlan(), req, sink)
	if err == nil {
		t.Fatal("expected an error when every tier fails")
	}
	if started {
		t.Error("a fully-exhausted plan must never report started=true")
	}
	if target.Provider != "mock-ctxfail-b" {
		t.Errorf("target.Provider = %q, want mock-ctxfail-b (the last tier attempted, not an empty target)", target.Provider)
	}
}

func TestClassifyUpstreamErrContextLengthExceeded(t *testing.T) {
	err := &providers.Error{Status: 400, Retryable: false, Code: providers.ErrCodeContextLengthExceeded, Message: "too big"}
	code, typ := classifyUpstreamErr(err)
	if code != http.StatusBadRequest || typ != "invalid_request_error" {
		t.Errorf("got (%d, %q), want (400, invalid_request_error)", code, typ)
	}
}

func TestClassifyUpstreamErrModelNotFound(t *testing.T) {
	err := &providers.Error{Status: 404, Retryable: false, Code: providers.ErrCodeModelNotFound, Message: "gone"}
	code, typ := classifyUpstreamErr(err)
	if code != http.StatusBadRequest || typ != "invalid_request_error" {
		t.Errorf("got (%d, %q), want (400, invalid_request_error)", code, typ)
	}
}

func TestClassifyUpstreamErrMultimodalNotSupported(t *testing.T) {
	err := &providers.Error{Status: 400, Retryable: false, Code: providers.ErrCodeMultimodalNotSupported, Message: "no vision"}
	code, typ := classifyUpstreamErr(err)
	if code != http.StatusBadRequest || typ != "invalid_request_error" {
		t.Errorf("got (%d, %q), want (400, invalid_request_error)", code, typ)
	}
}

func TestClassifyUpstreamErrUnrecognizedStaysUpstreamError(t *testing.T) {
	err := &providers.Error{Status: 400, Retryable: false, Message: "malformed"}
	code, typ := classifyUpstreamErr(err)
	if code != http.StatusBadGateway || typ != "upstream_error" {
		t.Errorf("got (%d, %q), want (502, upstream_error) — unrecognized errors must keep today's mapping", code, typ)
	}
}

func TestClassifyUpstreamErrAllBusyUnaffected(t *testing.T) {
	code, typ := classifyUpstreamErr(errAllBusy)
	if code != http.StatusTooManyRequests || typ != "rate_limit_error" {
		t.Errorf("got (%d, %q), want (429, rate_limit_error) — errAllBusy mapping must be untouched", code, typ)
	}
}
