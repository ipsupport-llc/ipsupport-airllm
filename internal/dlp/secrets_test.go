package dlp

import (
	"strings"
	"testing"
)

const opensshKey = `-----BEGIN OPENSSH PRIVATE KEY-----
b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAMwAAAAtzc2gtZW
QyNTUxOQAAACDj. fakefakefakefakefakefakefakefakefakefakefakefake
-----END OPENSSH PRIVATE KEY-----`

const rsaKey = `-----BEGIN RSA PRIVATE KEY-----
MIIEogIBAAKCAQEA0fakefakefakefakefakefakefakefakefakefakefakefake
-----END RSA PRIVATE KEY-----`

const ecKey = `-----BEGIN EC PRIVATE KEY-----
MHcCAQEEIfakefakefakefakefakefakefakefakefakefake
-----END EC PRIVATE KEY-----`

const sslKey = `-----BEGIN PRIVATE KEY-----
MIIEvQIBADANBgkqhkiG9w0BAQEFAASfakefakefakefakefakefakefakefake
-----END PRIVATE KEY-----`

func TestDetectSecretFamilies(t *testing.T) {
	cases := []struct {
		name, label, text string
	}{
		{"openssh", "private_key", "key:\n" + opensshKey},
		{"rsa", "private_key", "key:\n" + rsaKey},
		{"ec", "private_key", "key:\n" + ecKey},
		{"ssl", "private_key", "tls cert key:\n" + sslKey},
		{"aws", "aws_access_key", "export AWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE"},
		{"gcp", "google_api_key", "key=AIza" + strings.Repeat("a", 35)},
		{"slack", "slack_token", "token xoxb-123456789012-AbCdEfGhIj done"},
		{"stripe", "stripe_key", "stripe sk_live_0123456789abcdEFGH end"},
		{"github", "github_token", "ghp_" + strings.Repeat("a", 40)},
		{"openai", "openai_key", "OPENAI_API_KEY=sk-proj-abcDEFghijklmnopqrstuvwx0123"},
		{"anthropic", "anthropic_key", "ANTHROPIC_API_KEY=sk-ant-api03-abcDEFghij0123456789xyz"},
		{"xai", "xai_key", "XAI=xai-abcDEFghij0123456789klmno"},
		{"bearer", "bearer_token", "Authorization: Bearer abcDEFghij0123456789klmnop"},
		{"jwt", "jwt", "tok eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJVabc"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs := Scan(c.text)
			if !hasLabel(fs, c.label) {
				t.Errorf("expected %s in %q; got %v", c.label, c.text, Labels(fs))
			}
		})
	}
}

func TestNoFalsePositives(t *testing.T) {
	// A git SHA (40 hex, single case), a UUID, and prose must NOT trip DLP.
	benign := "Commit 9f1c2e4a8b3d5f6071829304a5b6c7d8e9f0a1b2 closes ticket " +
		"550e8400-e29b-41d4-a716-446655440000 in the auth module; see the docs."
	if fs := Scan(benign); len(fs) != 0 {
		t.Errorf("benign text flagged: %v", Labels(fs))
	}
}

func TestRedactMultiple(t *testing.T) {
	openai := "sk-proj-abcDEFghijklmnopqrstuvwx0123"
	aws := "AKIAIOSFODNN7EXAMPLE"
	text := "keys: " + openai + " and " + aws + " ok"
	out := Redact(text, Scan(text))
	if strings.Contains(out, openai) || strings.Contains(out, aws) {
		t.Errorf("a secret survived redaction: %q", out)
	}
	if strings.Count(out, "[REDACTED:") != 2 {
		t.Errorf("expected 2 redaction markers, got %q", out)
	}
}
