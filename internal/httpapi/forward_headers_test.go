package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/config"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/metrics"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/providers"
)

func TestWithForwardedHeadersSelectsOnlyAllowedNamesCaseInsensitively(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set("X-Request-Id", "abc-123")
	r.Header.Set("X-Not-Allowed", "nope")

	ctx := withForwardedHeaders(r.Context(), r, []string{"x-request-id"}) // lowercase on purpose
	got := providers.ForwardedHeadersFromContext(ctx)
	if got.Get("X-Request-Id") != "abc-123" {
		t.Errorf("X-Request-Id = %q, want abc-123", got.Get("X-Request-Id"))
	}
	if got.Get("X-Not-Allowed") != "" {
		t.Errorf("X-Not-Allowed leaked through despite not being in the allowlist: %q", got.Get("X-Not-Allowed"))
	}
}

func TestWithForwardedHeadersEmptyAllowlistIsNoop(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set("X-Request-Id", "abc-123")
	ctx := withForwardedHeaders(r.Context(), r, nil)
	if ctx != r.Context() {
		t.Error("an empty allowlist must leave ctx unchanged")
	}
}

// TestForwardHeadersReachHandlerContext proves ServeHTTP builds the
// forwarded-headers context from the configured allowlist, mirroring
// TestSessionHeaderReachesTheHandler for the existing X-Session-Id carrier.
func TestForwardHeadersReachHandlerContext(t *testing.T) {
	s := &Server{mux: http.NewServeMux(), metrics: metrics.New(), cfg: &config.Config{ForwardHeaders: []string{"X-Request-Id"}}}
	var seen string
	s.mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		seen = providers.ForwardedHeadersFromContext(r.Context()).Get("X-Request-Id")
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set("X-Request-Id", "req-42")
	s.ServeHTTP(httptest.NewRecorder(), r)
	if seen != "req-42" {
		t.Errorf("forwarded header in handler = %q, want req-42", seen)
	}
}
