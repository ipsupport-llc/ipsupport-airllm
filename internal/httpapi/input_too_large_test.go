package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/llm"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/providers"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/routing"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/unavail"
)

// TestEmbeddingInputTooLargeIsAClientError: llama.cpp's server answers an
// embedding input longer than its physical batch with a 500 server_error
// "input (N tokens) is too large to process". That is the request's fault,
// so the client must get a 400, and the target must stay available instead
// of being marked down for everyone: a second request still reaches it.
func TestEmbeddingInputTooLargeIsAClientError(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"code":500,"message":"input (8582 tokens) is too large to process. increase the physical batch size (current batch size: 8192)","type":"server_error"}}`))
	}))
	t.Cleanup(up.Close)

	clk := newFakeClock()
	s := newRunChatTestServer(t, providers.NewOpenAICompat("llama", "openai", up.URL, ""))
	s.unavail = unavail.New(nil).WithClock(clk.Now)
	plan := &routing.Plan{Alias: "bge-m3", Tiers: [][]routing.Target{
		{{Provider: "llama", UpstreamModel: "bge-m3"}},
	}}

	for i := 1; i <= 2; i++ {
		_, _, err := s.runEmbeddings(context.Background(), plan, llm.EmbeddingsRequest{Model: "bge-m3", Input: []string{"long"}})
		if code, _ := classifyUpstreamErr(err); code != http.StatusBadRequest {
			t.Fatalf("request %d: %v maps to %d, want 400", i, err, code)
		}
	}
	// Both requests reached the upstream: the first one did not mark the
	// target unavailable (which would have turned the second into a 503
	// without a call).
	if got := calls.Load(); got != 2 {
		t.Errorf("upstream called %d times, want 2 (the target must not be marked unavailable)", got)
	}
	if _, skip := s.unavail.Check(context.Background(), unavail.Key{Provider: "llama", UpstreamModel: "bge-m3"}); skip {
		t.Error("target marked unavailable after an input-too-large error")
	}
}
