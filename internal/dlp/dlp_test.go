package dlp

import (
	"strings"
	"testing"
)

func hasLabel(fs []Finding, label string) bool {
	for _, f := range fs {
		if f.Label == label {
			return true
		}
	}
	return false
}

func TestScanDetectsSecrets(t *testing.T) {
	cases := map[string]string{
		"openai_key":     "here is my key sk-proj-abcdefghijklmnopqrstuvwxyz0123 thanks",
		"aws_access_key": "creds AKIAIOSFODNN7EXAMPLE end",
		"github_token":   "token ghp_0123456789012345678901234567890123456789 ok",
		"jwt":            "auth eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJVabc done",
	}
	for label, text := range cases {
		t.Run(label, func(t *testing.T) {
			if !hasLabel(Scan(text), label) {
				t.Errorf("expected to detect %s in %q; got %+v", label, text, Scan(text))
			}
		})
	}
}

func TestScanDetectsPrivateKey(t *testing.T) {
	pem := "-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEArandombase64stuff\n-----END RSA PRIVATE KEY-----"
	if !hasLabel(Scan("config:\n"+pem), "private_key") {
		t.Error("expected to detect a PEM private key")
	}
}

func TestScanIgnoresProse(t *testing.T) {
	text := "The quick brown fox jumps over the lazy dog while writing some code today."
	if fs := Scan(text); len(fs) != 0 {
		t.Errorf("expected no findings in prose, got %+v", fs)
	}
}

func TestHighEntropy(t *testing.T) {
	text := "the value is Zm9vYmFyYmF6cXV4MTIzNDU2Nzg5MGFiY2RlZmdoaWprbA right here"
	if !hasLabel(Scan(text), "high_entropy") {
		t.Errorf("expected high_entropy finding, got %+v", Scan(text))
	}
}

func TestRedactRemovesSecret(t *testing.T) {
	key := "sk-proj-abcdefghijklmnopqrstuvwxyz0123"
	text := "use " + key + " now"
	out := Redact(text, Scan(text))
	if strings.Contains(out, key) {
		t.Errorf("redacted output still contains the secret: %q", out)
	}
	if !strings.Contains(out, "[REDACTED:") {
		t.Errorf("expected a redaction marker, got %q", out)
	}
}

func TestRedactNoFindings(t *testing.T) {
	text := "nothing sensitive here"
	if got := Redact(text, Scan(text)); got != text {
		t.Errorf("Redact altered clean text: %q", got)
	}
}
