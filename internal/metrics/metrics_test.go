package metrics

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestNewRegistersAndRecords(t *testing.T) {
	m := New()
	m.RecordRequest("openai", 200, 12*time.Millisecond)
	m.RecordRequest("openai", 200, 8*time.Millisecond)
	m.ObserveComponent("provider", 5*time.Millisecond)
	m.RecordUsage("openai", 10, 20, 0.001)
	m.IncRateLimited("usage_limit")
	m.DLPModelInc()
	m.DLPModelDone(3 * time.Millisecond)

	if got := testutil.ToFloat64(m.httpRequests.WithLabelValues("openai", "200")); got != 2 {
		t.Errorf("http_requests_total{openai,200} = %v, want 2", got)
	}
	if got := testutil.ToFloat64(m.tokens.WithLabelValues("openai", "prompt")); got != 10 {
		t.Errorf("tokens prompt = %v, want 10", got)
	}
	if got := testutil.ToFloat64(m.tokens.WithLabelValues("openai", "completion")); got != 20 {
		t.Errorf("tokens completion = %v, want 20", got)
	}
	if got := testutil.ToFloat64(m.rateLimited.WithLabelValues("usage_limit")); got != 1 {
		t.Errorf("rate_limited{usage_limit} = %v, want 1", got)
	}
}

func TestNilSafe(t *testing.T) {
	var m *Metrics // nil
	// Must not panic.
	m.RecordRequest("openai", 200, time.Millisecond)
	m.ObserveComponent("dlp", time.Millisecond)
	m.RecordUsage("anthropic", 1, 1, 0.0)
	m.IncRateLimited("provider_busy")
	m.DLPModelInc()
	m.DLPModelDone(time.Millisecond)
	m.RegisterCaptureDropped(func() float64 { return 0 })
}

func TestRegisterCaptureDropped(t *testing.T) {
	m := New()
	m.RegisterCaptureDropped(func() float64 { return 7 })
	got, err := testutil.GatherAndCount(m.reg, "airllm_capture_dropped")
	if err != nil || got != 1 {
		t.Fatalf("capture_dropped gauge not registered: count=%d err=%v", got, err)
	}
}
