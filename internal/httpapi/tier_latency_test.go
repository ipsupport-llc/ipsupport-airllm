package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/llm"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/providers"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/routing"
)

// slowTailUpstream streams one content chunk at once and then holds the
// stream open for tail before ending it.
func slowTailUpstream(t *testing.T, tail time.Duration) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\n", contentDelta("Paris"))
		w.(http.Flusher).Flush()
		time.Sleep(tail)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestTierLatencyOfAStreamIsTimeToFirstChunk(t *testing.T) {
	up := slowTailUpstream(t, 700*time.Millisecond)
	s := newRunChatTestServer(t, providers.NewOpenAICompat("tier", "openai", up.URL, ""), providers.NewMock("mock-ok"))
	plan := budgetPlan(routing.Target{Provider: "tier", UpstreamModel: "m", Options: routing.TargetOptions{TimeoutMS: ms(2000)}})

	res, _, _, err := s.runStream(context.Background(), plan, llm.ChatRequest{Stream: true}, &recordingSink{})
	if err != nil || res.Provider != "tier" {
		t.Fatalf("served=%q err=%v, want the first tier", res.Provider, err)
	}

	m := scrapeMetrics(t, s)
	for _, want := range []string{
		`airllm_tier_attempt_duration_seconds_count{alias="voice-reply",outcome="success",tier="0"} 1`,
		// The long tail after the first chunk is not the tier's latency.
		`airllm_tier_attempt_duration_seconds_bucket{alias="voice-reply",outcome="success",tier="0",le="0.5"} 1`,
	} {
		if !strings.Contains(m, want) {
			t.Errorf("metrics lack %s\n%s", want, grepLines(m, "airllm_tier_attempt"))
		}
	}
}

func TestTierLatencyIsRecordedPerOutcome(t *testing.T) {
	up := newSwitchableUpstream(t, true)
	s := newRunChatTestServer(t, providers.NewOpenAICompat("flaky", "openai", up.URL, ""), providers.NewMock("mock-ok"))
	plan := guardedPlan("voice-reply", "flaky", routing.TargetOptions{})

	chat(t, s, plan)

	m := scrapeMetrics(t, s)
	for _, want := range []string{
		`airllm_tier_attempt_duration_seconds_count{alias="voice-reply",outcome="failure",tier="0"} 1`,
		`airllm_tier_attempt_duration_seconds_count{alias="voice-reply",outcome="success",tier="1"} 1`,
	} {
		if !strings.Contains(m, want) {
			t.Errorf("metrics lack %s\n%s", want, grepLines(m, "airllm_tier_attempt"))
		}
	}
}
