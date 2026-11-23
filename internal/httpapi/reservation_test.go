package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/ledger"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/limits"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/llm"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/metrics"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/policy"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/pricing"
)

// newUsageTestServer is a Server with what finalizeUsage touches: a ledger
// (never started — entries just queue), pricing, metrics and a limiter on rdb.
func newUsageTestServer(rdb *redis.Client) *Server {
	return &Server{
		ledger: ledger.New(nil), pricing: pricing.New(), metrics: metrics.New(),
		limiter: limits.New(rdb),
	}
}

// reservationRig connects to TEST_REDIS_URL (skipping without it) and
// returns a server plus a key whose token limit is 1000 per 5h.
func reservationRig(t *testing.T) (*Server, authedKey) {
	t.Helper()
	dsn := os.Getenv("TEST_REDIS_URL")
	if dsn == "" {
		t.Skip("TEST_REDIS_URL not set; skipping limiter integration test")
	}
	opt, err := redis.ParseURL(dsn)
	if err != nil {
		t.Fatal(err)
	}
	rdb := redis.NewClient(opt)
	t.Cleanup(func() { _ = rdb.Close() })
	ak := authedKey{
		KeyID:  fmt.Sprintf("reservation-test-%d", time.Now().UnixNano()),
		Policy: policy.KeyPolicy{Limits: []byte(`{"tokens":{"5h":1000}}`)},
	}
	return newUsageTestServer(rdb), ak
}

func mustAllow(t *testing.T, s *Server, ak authedKey, reserve int64, want bool) limits.Decision {
	t.Helper()
	_, denied, dec := s.limitDenied(context.Background(), ak, reserve)
	if denied == want {
		t.Fatalf("limit check reserving %d: denied=%v, want allowed=%v", reserve, denied, want)
	}
	return dec
}

// TestFinalizeRefundsAfterClientCancel: a client that hung up cancels the
// request context; the reservation must still be refunded, not stuck until
// the window ends.
func TestFinalizeRefundsAfterClientCancel(t *testing.T) {
	s, ak := reservationRig(t)
	dec := mustAllow(t, s, ak, 1000, true)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.finalizeUsage(ctx, ledger.Entry{Status: http.StatusBadGateway}, ak.KeyID, "m", llm.Usage{}, &dec)
	mustAllow(t, s, ak, 1000, true)
}

// TestReleaseRefundsUnsettledReservation: an exit that never reaches
// finalizeUsage (a DLP block) gives the headroom back.
func TestReleaseRefundsUnsettledReservation(t *testing.T) {
	s, ak := reservationRig(t)
	dec := mustAllow(t, s, ak, 1000, true)
	s.releaseReservation(context.Background(), ak.KeyID, &dec)
	mustAllow(t, s, ak, 1000, true)
}

// TestReservationSettledExactlyOnce: after finalizeUsage settled it, the
// deferred release must not refund the reservation a second time.
func TestReservationSettledExactlyOnce(t *testing.T) {
	s, ak := reservationRig(t)
	dec := mustAllow(t, s, ak, 1000, true)
	s.finalizeUsage(context.Background(), ledger.Entry{Status: http.StatusOK}, ak.KeyID, "m", llm.Usage{PromptTokens: 100, CompletionTokens: 100}, &dec)
	s.releaseReservation(context.Background(), ak.KeyID, &dec)
	// 200 used: an 800 reservation fits and then the window is full. A
	// double refund would have left room for more.
	mustAllow(t, s, ak, 800, true)
	mustAllow(t, s, ak, 1, false)
}

// TestFinalizeUsageReturnsCost: the finalized entry carries the cost, so
// captures stop recording $0 for priced chat traffic.
func TestFinalizeUsageReturnsCost(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	s := newUsageTestServer(rdb)
	s.pricing.Set("p", "m", pricing.Price{InputPer1M: 1, OutputPer1M: 2, Unit: pricing.UnitTokens})
	e := s.finalizeUsage(context.Background(), ledger.Entry{ProviderName: "p", Status: http.StatusOK}, "k", "m", llm.Usage{PromptTokens: 1_000_000, CompletionTokens: 1_000_000}, nil)
	if e.CostUSD != 3 {
		t.Errorf("CostUSD = %v, want 3", e.CostUSD)
	}
}
