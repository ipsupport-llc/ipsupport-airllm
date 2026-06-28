package dlp

import (
	"regexp"
	"testing"
)

func TestPIIOffByDefault(t *testing.T) {
	// Scan() uses defaults: PII patterns must be off, so none of these flag.
	for _, s := range []string{
		"email john@example.com",
		"call 555-123-4567",
		"ssn 123-45-6789",
		"card 4111111111111111",
		"host 192.168.1.1",
	} {
		if fs := Scan(s); len(fs) != 0 {
			t.Errorf("Scan(%q) flagged PII by default: %+v", s, fs)
		}
	}
}

func TestEmailPattern(t *testing.T) {
	on := PatternSet{Enabled: map[string]bool{"email": true}}
	if fs := ScanWith("contact john.doe+x@sub.example.com please", on); !hasLabel(fs, "email") {
		t.Errorf("expected email finding, got %+v", fs)
	}
	if fs := ScanWith("no address here at all", on); hasLabel(fs, "email") {
		t.Error("plain prose must not flag email")
	}
}

func TestSSNPattern(t *testing.T) {
	on := PatternSet{Enabled: map[string]bool{"ssn": true}}
	if fs := ScanWith("ssn 123-45-6789 ok", on); !hasLabel(fs, "ssn") {
		t.Errorf("expected ssn finding, got %+v", fs)
	}
	if fs := ScanWith("order 12-3456 ok", on); hasLabel(fs, "ssn") {
		t.Error("non-SSN digit groups must not flag ssn")
	}
}

func TestCreditCardLuhn(t *testing.T) {
	on := PatternSet{Enabled: map[string]bool{"credit_card": true}}
	// 4111111111111111 is a valid Luhn test number.
	if fs := ScanWith("card 4111111111111111 end", on); !hasLabel(fs, "credit_card") {
		t.Errorf("expected credit_card for a valid Luhn number, got %+v", fs)
	}
	// Same length, deliberately bad checksum.
	if fs := ScanWith("num 4111111111111112 end", on); hasLabel(fs, "credit_card") {
		t.Error("a number failing Luhn must not flag credit_card")
	}
}

func TestIPv4Octets(t *testing.T) {
	on := PatternSet{Enabled: map[string]bool{"ip_address": true}}
	if fs := ScanWith("host 192.168.1.1 up", on); !hasLabel(fs, "ip_address") {
		t.Errorf("expected ip_address, got %+v", fs)
	}
	if fs := ScanWith("ver 999.999.999.999 x", on); hasLabel(fs, "ip_address") {
		t.Error("out-of-range octets must not flag ip_address")
	}
}

func TestPhonePattern(t *testing.T) {
	on := PatternSet{Enabled: map[string]bool{"phone": true}}
	for _, s := range []string{"call +1 (555) 123-4567", "ring 555-123-4567 now", "num 5551234567"} {
		if fs := ScanWith(s, on); !hasLabel(fs, "phone") {
			t.Errorf("expected phone for %q, got %+v", s, fs)
		}
	}
}

func TestCustomPattern(t *testing.T) {
	ps := PatternSet{Custom: []CustomPattern{{Label: "ticket", Re: regexp.MustCompile(`TCKT-\d{4}`)}}}
	if fs := ScanWith("see TCKT-2026 for details", ps); !hasLabel(fs, "ticket") {
		t.Errorf("expected custom 'ticket' finding, got %+v", fs)
	}
}

func TestScanWithSkipsZeroWidthCustom(t *testing.T) {
	// A zero-width-matching custom regex must not emit findings (defense in
	// depth; the httpapi layer also rejects such patterns at save time).
	ps := PatternSet{Custom: []CustomPattern{{Label: "z", Re: regexp.MustCompile(`x*`)}}}
	if fs := ScanWith("hello world", ps); len(fs) != 0 {
		t.Errorf("zero-width custom matches must be skipped, got %+v", fs)
	}
}

func TestDisabledSecretNotFlagged(t *testing.T) {
	// Explicitly disable openai_key; entropy off; the key must not be flagged.
	ps := PatternSet{Enabled: map[string]bool{"openai_key": false}}
	if fs := ScanWith("key sk-proj-abcDEFghij0123456789 end", ps); len(fs) != 0 {
		t.Errorf("disabled openai_key must produce no finding, got %+v", fs)
	}
	// With it enabled (default), it flags.
	if fs := ScanWith("key sk-proj-abcDEFghij0123456789 end", PatternSet{}); !hasLabel(fs, "openai_key") {
		t.Errorf("openai_key on by default, got %+v", fs)
	}
}

func TestEntropyToggle(t *testing.T) {
	s := "v Zm9vYmFyQmF6MTIzNDU2Nzg5MGFiQ0RlZmdoSWprTA end"
	if fs := ScanWith(s, PatternSet{Entropy: false}); hasLabel(fs, "high_entropy") {
		t.Error("entropy off must not flag high_entropy")
	}
	if fs := ScanWith(s, PatternSet{Entropy: true}); !hasLabel(fs, "high_entropy") {
		t.Error("entropy on must flag the high-entropy blob")
	}
}

func TestBuiltinPatternsMetadata(t *testing.T) {
	infos := BuiltinPatterns()
	byLabel := map[string]PatternInfo{}
	for _, p := range infos {
		byLabel[p.Label] = p
	}
	if p, ok := byLabel["openai_key"]; !ok || p.Category != "secret" || !p.DefaultOn {
		t.Errorf("openai_key should be a default-on secret, got %+v ok=%v", p, ok)
	}
	if p, ok := byLabel["email"]; !ok || p.Category != "pii" || p.DefaultOn {
		t.Errorf("email should be an opt-in pii pattern, got %+v ok=%v", p, ok)
	}
	if _, ok := byLabel["high_entropy"]; !ok {
		t.Error("high_entropy must be listed as a toggleable pattern")
	}
}
