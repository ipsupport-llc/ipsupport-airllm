package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/dlp"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/llm"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/modelpool"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/store"
)

func TestCompileCustomPatterns(t *testing.T) {
	// Enabled valid + disabled (skipped) + the disabled one's regex is ignored.
	got, err := compileCustomPatterns([]customPattern{
		{Label: "ticket", Regex: `TCKT-\d{4}`, Enabled: true},
		{Label: "off", Regex: `whatever`, Enabled: false},
	})
	if err != nil {
		t.Fatalf("valid set: %v", err)
	}
	if len(got) != 1 || got[0].Label != "ticket" {
		t.Fatalf("expected only the enabled pattern, got %+v", got)
	}
}

func TestCompileCustomPatternsRejectsBadRegex(t *testing.T) {
	if _, err := compileCustomPatterns([]customPattern{{Label: "x", Regex: "(", Enabled: true}}); err == nil {
		t.Fatal("an uncompilable regex must be rejected")
	}
}

func TestCompileCustomPatternsRejectsEmptyLabel(t *testing.T) {
	if _, err := compileCustomPatterns([]customPattern{{Label: "  ", Regex: "x", Enabled: true}}); err == nil {
		t.Fatal("an empty label must be rejected")
	}
}

func TestCompileCustomPatternsRejectsOverLong(t *testing.T) {
	long := strings.Repeat("a", maxCustomRegexLen+1)
	if _, err := compileCustomPatterns([]customPattern{{Label: "x", Regex: long, Enabled: true}}); err == nil {
		t.Fatal("an over-long regex must be rejected")
	}
}

func TestCompileCustomPatternsRejectsEmptyMatch(t *testing.T) {
	// A regex that matches the empty string would emit a zero-width finding at
	// every position and corrupt the redacted prompt.
	for _, re := range []string{`\d*`, `.*`, `a?`} {
		if _, err := compileCustomPatterns([]customPattern{{Label: "x", Regex: re, Enabled: true}}); err == nil {
			t.Errorf("empty-matching regex %q must be rejected", re)
		}
	}
}

func TestCompileCustomPatternsRejectsLongLabel(t *testing.T) {
	long := strings.Repeat("L", maxCustomLabelLen+1)
	if _, err := compileCustomPatterns([]customPattern{{Label: long, Regex: "x", Enabled: true}}); err == nil {
		t.Fatal("an over-long label must be rejected")
	}
}

func TestCompileCustomPatternsKeepsValidSkipsInvalid(t *testing.T) {
	// loadDLP robustness: a bad entry must not drop the valid ones that follow.
	got, err := compileCustomPatterns([]customPattern{
		{Label: "good1", Regex: `AAA-\d+`, Enabled: true},
		{Label: "bad", Regex: `(`, Enabled: true},
		{Label: "good2", Regex: `BBB-\d+`, Enabled: true},
	})
	if err == nil {
		t.Fatal("expected an error reporting the invalid entry")
	}
	if len(got) != 2 {
		t.Fatalf("expected the 2 valid patterns to survive, got %d", len(got))
	}
}

func TestModelToggleNormalization(t *testing.T) {
	// The sidecar may emit full-word entities; the toggle must still apply.
	in := []dlp.Finding{{Label: "pii:PERSON", Start: 0, End: 1}, {Label: "pii:LOCATION", Start: 2, End: 3}}
	got := filterModelFindings(append([]dlp.Finding(nil), in...), map[string]bool{"person_name": false, "address": true})
	if len(got) != 1 || got[0].Label != "pii:LOCATION" {
		t.Fatalf("PERSON should drop (person_name off), LOCATION keep (address on); got %+v", got)
	}
}

func TestCompileCustomPatternsRejectsTooMany(t *testing.T) {
	many := make([]customPattern, maxCustomPatterns+1)
	for i := range many {
		many[i] = customPattern{Label: "x", Regex: "y", Enabled: true}
	}
	if _, err := compileCustomPatterns(many); err == nil {
		t.Fatal("too many custom patterns must be rejected")
	}
}

func TestValidatePatternLabels(t *testing.T) {
	if err := validatePatternLabels(nil); err != nil {
		t.Errorf("nil map must be valid: %v", err)
	}
	ok := map[string]bool{"openai_key": true, "email": false, "person_name": true, "high_entropy": true}
	if err := validatePatternLabels(ok); err != nil {
		t.Errorf("known labels must pass: %v", err)
	}
	if err := validatePatternLabels(map[string]bool{"emial": true}); err == nil {
		t.Error("an unknown/typo label must be rejected")
	}
}

func TestDlpToggle(t *testing.T) {
	m := map[string]bool{"a": true, "b": false}
	if !dlpToggle(m, "a", false) {
		t.Error("explicit true must win")
	}
	if dlpToggle(m, "b", true) {
		t.Error("explicit false must win")
	}
	if !dlpToggle(m, "c", true) {
		t.Error("absent label must use the default (true)")
	}
	if !dlpToggle(nil, "c", true) {
		t.Error("nil map must use the default")
	}
}

func TestFilterModelFindings(t *testing.T) {
	in := []dlp.Finding{
		{Label: "pii:PER", Start: 0, End: 1},
		{Label: "pii:LOC", Start: 2, End: 3},
		{Label: "pii:ORG", Start: 4, End: 5},
	}
	// nil map => keep all (legacy behavior).
	if got := filterModelFindings(append([]dlp.Finding(nil), in...), nil); len(got) != 3 {
		t.Fatalf("nil toggles must keep all model findings, got %d", len(got))
	}
	// Only person_name enabled => only pii:PER survives.
	got := filterModelFindings(append([]dlp.Finding(nil), in...), map[string]bool{"person_name": true})
	if len(got) != 1 || got[0].Label != "pii:PER" {
		t.Fatalf("expected only pii:PER, got %+v", got)
	}
}

// newDLPEnforceTestServer builds a minimal *Server wired for dlpEnforce: DLP +
// model scanning enabled, model URLs pointed at the given sidecar, and a store
// backed by an unreachable (but structurally valid) Postgres pool. dlpEnforce's
// best-effort incident/webhook lookups then fail with a connection error
// (fast: localhost connection-refused) instead of a nil-pointer panic, so the
// test needs no real database.
func newDLPEnforceTestServer(t *testing.T, sidecarURL string) *Server {
	t.Helper()
	pgPool, err := pgxpool.New(context.Background(), "postgres://user:pass@127.0.0.1:1/db?connect_timeout=1")
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pgPool.Close)

	cfg := dlpConfig{
		Enabled:       true,
		Action:        "flag",
		ModelEnabled:  true,
		ModelURLs:     []string{sidecarURL},
		ModelMinScore: 0.5,
	}
	s := &Server{
		st:    &store.Store{PG: pgPool},
		httpc: http.DefaultClient,
	}
	s.dlpPtr.Store(&cfg)
	s.modelPool = modelpool.New(
		func() ([]string, int) { return []string{sidecarURL}, 0 },
		func(host string) ([]string, error) {
			t.Fatalf("unexpected DNS resolution for host %q; the test sidecar is an IP literal", host)
			return nil, nil
		},
	)
	return s
}

// TestDlpEnforceModelScanGate proves modelScan gates ONLY the layer-2 BERT
// sidecar call: with model scanning enabled config-wise, dlpEnforce(...,
// false) must never hit the sidecar, while the layer-1 deterministic scan
// still fires on a message containing a detectable secret. The true case is
// a positive control proving the same setup DOES hit the sidecar when the
// alias opts in, so the false-case result isn't just an artifact of a
// misconfigured pool.
func TestDlpEnforceModelScanGate(t *testing.T) {
	const secret = "sk-ant-api03-aaaabbbbccccddddeeee1234"

	for _, tc := range []struct {
		name      string
		modelScan bool
		wantHits  int32
	}{
		{"modelScan=false skips sidecar", false, 0},
		{"modelScan=true hits sidecar", true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var hits atomic.Int32
			sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"findings": []any{}})
			}))
			defer sidecar.Close()

			s := newDLPEnforceTestServer(t, sidecar.URL)
			req := &llm.ChatRequest{Messages: []llm.Message{{Role: "user", Content: "key: " + secret}}}

			blocked, _, result := s.dlpEnforce(context.Background(), authedKey{KeyID: "k1", UserID: "u1"}, "openai", req, tc.modelScan)

			if blocked {
				t.Error("action=flag must never block")
			}
			if !result.HadIncident || len(result.Findings) == 0 {
				t.Fatalf("expected layer-1 findings on a message with a detectable secret, got %+v", result)
			}
			found := false
			for _, f := range result.Findings {
				if f.Label == "anthropic_key" {
					found = true
				}
			}
			if !found {
				t.Errorf("expected an anthropic_key finding, got %+v", result.Findings)
			}
			if got := hits.Load(); got != tc.wantHits {
				t.Errorf("sidecar hits = %d, want %d", got, tc.wantHits)
			}
		})
	}
}
