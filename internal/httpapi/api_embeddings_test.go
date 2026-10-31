package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/llm"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/metrics"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/providers"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/routing"
)

// These two exercise handleEmbeddings' decode/policy gate — the part that
// runs before router.Resolve, which needs a real database this environment
// does not have (see testPool). The router/provider wiring is covered at
// the runEmbeddings level above instead.

func TestHandleEmbeddingsRejectsBadJSON(t *testing.T) {
	s := &Server{}
	r := asKey(httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(`{not json`)), "embed")
	rec := httptest.NewRecorder()
	s.handleEmbeddings(rec, r)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleEmbeddingsRejectsDisallowedModel(t *testing.T) {
	s := &Server{}
	r := asKey(httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(`{"model":"embed","input":"hi"}`)), "some-other-alias")
	rec := httptest.NewRecorder()
	s.handleEmbeddings(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

// TestRunEmbeddingsHappyPath drives runEmbeddings against a mock provider
// through a one-tier plan, mirroring failover_test.go's style for the chat
// and audio exec paths.
func TestRunEmbeddingsHappyPath(t *testing.T) {
	reg := providers.NewRegistry()
	reg.Register(providers.NewMock("mock"), 0)
	s := &Server{router: routing.NewRouter(nil), metrics: metrics.New()}
	s.regPtr.Store(reg)
	plan := &routing.Plan{Alias: "embed", Tiers: [][]routing.Target{
		{{Provider: "mock", UpstreamModel: "mock-embed"}},
	}}

	resp, res, err := s.runEmbeddings(context.Background(), plan, llm.EmbeddingsRequest{Model: "embed", Input: []string{"a", "b"}})
	if err != nil {
		t.Fatalf("runEmbeddings: %v", err)
	}
	if res.Provider != "mock" || res.UpstreamModel != "mock-embed" {
		t.Errorf("res = %+v", res)
	}
	if len(resp.Data) != 2 {
		t.Fatalf("len(resp.Data) = %d, want 2", len(resp.Data))
	}
}

// chatOnlyNoEmbed is a provider that can chat but has no embeddings
// capability — used to prove runEmbeddings falls through to the next tier
// instead of resolving here, mirroring chatOnly/
// TestAudioCapableButBusyTargetIsReportedAsBusy's pattern for the audio
// capability check.
type chatOnlyNoEmbed struct{ name string }

func (e chatOnlyNoEmbed) Name() string     { return e.name }
func (e chatOnlyNoEmbed) Kind() string     { return "openai" }
func (e chatOnlyNoEmbed) Protocol() string { return "openai" }
func (e chatOnlyNoEmbed) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}
func (e chatOnlyNoEmbed) ChatStream(context.Context, llm.ChatRequest, func(llm.StreamChunk) error) error {
	return nil
}

func TestRunEmbeddingsSkipsTargetWithoutEmbedder(t *testing.T) {
	reg := providers.NewRegistry()
	reg.Register(chatOnlyNoEmbed{name: "text-only"}, 0)
	reg.Register(providers.NewMock("mock"), 0)
	s := &Server{router: routing.NewRouter(nil), metrics: metrics.New()}
	s.regPtr.Store(reg)
	plan := &routing.Plan{Alias: "embed", Tiers: [][]routing.Target{
		{{Provider: "text-only", UpstreamModel: "m"}},
		{{Provider: "mock", UpstreamModel: "mock-embed"}},
	}}

	resp, res, err := s.runEmbeddings(context.Background(), plan, llm.EmbeddingsRequest{Model: "embed", Input: []string{"a"}})
	if err != nil {
		t.Fatalf("runEmbeddings: %v", err)
	}
	if res.Provider != "mock" {
		t.Errorf("served by %q, want mock (text-only must be skipped)", res.Provider)
	}
	if len(resp.Data) != 1 {
		t.Errorf("len(resp.Data) = %d, want 1", len(resp.Data))
	}
}

func TestRunEmbeddingsAllTiersFail(t *testing.T) {
	reg := providers.NewRegistry()
	reg.Register(providers.NewMock("mock"), 0)
	s := &Server{router: routing.NewRouter(nil), metrics: metrics.New()}
	s.regPtr.Store(reg)
	plan := &routing.Plan{Alias: "embed", Tiers: [][]routing.Target{
		{{Provider: "mock", UpstreamModel: "mock-embed-fail"}},
	}}

	_, _, err := s.runEmbeddings(context.Background(), plan, llm.EmbeddingsRequest{Model: "embed", Input: []string{"a"}})
	if code, _ := classifyUpstreamErr(err); code != http.StatusBadGateway {
		t.Errorf("got %v (status %d), want 502", err, code)
	}
}
