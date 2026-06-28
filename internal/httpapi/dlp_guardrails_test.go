package httpapi

import (
	"strings"
	"testing"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/dlp"
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
