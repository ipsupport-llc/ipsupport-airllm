package metrics

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/llm"
)

func TestNewRegistersAndRecords(t *testing.T) {
	m := New()
	m.RecordRequest("openai", 200, 12*time.Millisecond)
	m.RecordRequest("openai", 200, 8*time.Millisecond)
	m.ObserveComponent("provider", 5*time.Millisecond)
	m.RecordUsage("openai", llm.Usage{PromptTokens: 10, CompletionTokens: 20, TotalTokens: 30, ReasoningTokens: 8}, 0.001)
	m.IncRateLimited("usage_limit")
	m.DLPModelObserve(3 * time.Millisecond)
	m.DLPModelSkipped("all_busy")

	if got := testutil.ToFloat64(m.httpRequests.WithLabelValues("openai", "200")); got != 2 {
		t.Errorf("http_requests_total{openai,200} = %v, want 2", got)
	}
	if got := testutil.ToFloat64(m.tokens.WithLabelValues("openai", "prompt")); got != 10 {
		t.Errorf("tokens prompt = %v, want 10", got)
	}
	if got := testutil.ToFloat64(m.tokens.WithLabelValues("openai", "completion")); got != 20 {
		t.Errorf("tokens completion = %v, want 20", got)
	}
	// Reasoning is a breakdown of completion, not a fourth quantity: the
	// completion count above still holds every billed output token.
	if got := testutil.ToFloat64(m.tokens.WithLabelValues("openai", "reasoning")); got != 8 {
		t.Errorf("tokens reasoning = %v, want 8", got)
	}
	if got := testutil.ToFloat64(m.rateLimited.WithLabelValues("usage_limit")); got != 1 {
		t.Errorf("rate_limited{usage_limit} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.dlpSkipped.WithLabelValues("all_busy")); got != 1 {
		t.Errorf("dlp_model_skipped_total{all_busy} = %v, want 1", got)
	}
}

func TestNilSafe(t *testing.T) {
	var m *Metrics // nil
	// Must not panic.
	m.RecordRequest("openai", 200, time.Millisecond)
	m.ObserveComponent("dlp", time.Millisecond)
	m.RecordUsage("anthropic", llm.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2}, 0.0)
	m.IncRateLimited("provider_busy")
	m.DLPModelObserve(time.Millisecond)
	m.DLPModelSkipped("all_busy")
	m.RegisterModelInflight(func() float64 { return 0 })
	m.RegisterModelEndpoints(func() float64 { return 0 })
	m.RegisterCaptureDropped(func() float64 { return 0 })
}

func TestRegisterModelGauges(t *testing.T) {
	m := New()
	m.RegisterModelInflight(func() float64 { return 2 })
	m.RegisterModelEndpoints(func() float64 { return 3 })
	if got, err := testutil.GatherAndCount(m.reg, "airllm_dlp_model_requests_inflight"); err != nil || got != 1 {
		t.Fatalf("inflight gauge not registered: count=%d err=%v", got, err)
	}
	if got, err := testutil.GatherAndCount(m.reg, "airllm_dlp_model_endpoints"); err != nil || got != 1 {
		t.Fatalf("endpoints gauge not registered: count=%d err=%v", got, err)
	}
}

func TestRegisterCaptureDropped(t *testing.T) {
	m := New()
	m.RegisterCaptureDropped(func() float64 { return 7 })
	got, err := testutil.GatherAndCount(m.reg, "airllm_capture_dropped")
	if err != nil || got != 1 {
		t.Fatalf("capture_dropped gauge not registered: count=%d err=%v", got, err)
	}
}
