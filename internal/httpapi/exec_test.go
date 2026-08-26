package httpapi

import (
	"net/http/httptest"
	"testing"

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
