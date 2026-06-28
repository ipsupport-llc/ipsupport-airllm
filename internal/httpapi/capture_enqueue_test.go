package httpapi

import (
	"bytes"
	"testing"

	"github.com/rromenskyi/ipsupport-airllm/internal/dlp"
	"github.com/rromenskyi/ipsupport-airllm/internal/llm"
)

// TestCaptureBodyRedactsRegardlessOfDLPAction proves the redacted-by-default
// invariant: capture Redact=true + DLP action='flag' (messages NOT redacted
// in-place) + secret in prompt => the stored (pre-seal) body contains no raw
// secret. This is the core security property of the capture layer.
func TestCaptureBodyRedactsRegardlessOfDLPAction(t *testing.T) {
	secret := "sk-ant-api03-aaaabbbbccccddddeeee1234"
	content := "key: " + secret
	// Simulate DLP action='flag': messages carry raw content (not redacted).
	msgs := []llm.Message{{Role: "user", Content: content}}
	// Per-message findings as dlpEnforce would produce them.
	msgFindings := [][]dlp.Finding{
		{{Label: "anthropic_key", Start: 5, End: 5 + len(secret)}},
	}

	body := captureBody(msgs, "ok", true /* redact */, msgFindings)

	if bytes.Contains(body, []byte(secret)) {
		t.Error("stored body must not contain raw secret when Redact=true (regardless of DLP action)")
	}
	if !bytes.Contains(body, []byte("[REDACTED:")) {
		t.Error("stored body must contain [REDACTED:] marker when Redact=true")
	}
}

// TestCaptureBodyPreservesRawContentWhenRedactFalse verifies that when
// Redact=false the body is stored as-is (operator opted out of redaction).
func TestCaptureBodyPreservesRawContentWhenRedactFalse(t *testing.T) {
	secret := "sk-ant-api03-aaaabbbbccccddddeeee1234"
	msgs := []llm.Message{{Role: "user", Content: "key: " + secret}}
	msgFindings := [][]dlp.Finding{
		{{Label: "anthropic_key", Start: 5, End: 5 + len(secret)}},
	}

	body := captureBody(msgs, "ok", false /* redact */, msgFindings)

	if !bytes.Contains(body, []byte(secret)) {
		t.Error("stored body must preserve raw content when Redact=false")
	}
}

// TestCaptureBodyNoFindingsNoChange verifies that captureBody with Redact=true
// but no findings is a no-op (nothing to redact).
func TestCaptureBodyNoFindingsNoChange(t *testing.T) {
	content := "hello world"
	msgs := []llm.Message{{Role: "user", Content: content}}

	body := captureBody(msgs, "ok", true /* redact */, nil /* no findings */)

	if !bytes.Contains(body, []byte(content)) {
		t.Error("body without findings must be stored unmodified even with Redact=true")
	}
}
