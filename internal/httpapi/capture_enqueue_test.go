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

// TestCaptureBodyNoDoubleRedactionWhenAlreadyRedacted verifies that when DLP
// action="redact" has already masked message content in place (AlreadyRedacted=true),
// enqueueCapture passes needRedact=false to captureBody, preventing a second
// redaction pass from re-applying the original byte offsets to the now-longer
// "[REDACTED:label]" string, which would produce corrupted output.
func TestCaptureBodyNoDoubleRedactionWhenAlreadyRedacted(t *testing.T) {
	secret := "sk-ant-api03-aaaabbbbccccddddeeee1234"
	content := "key: " + secret
	findings := [][]dlp.Finding{
		{{Label: "anthropic_key", Start: 5, End: 5 + len(secret)}},
	}

	// Simulate what dlpEnforce does with action="redact": redact in place.
	alreadyRedacted := dlp.Redact(content, findings[0])
	if bytes.Contains([]byte(alreadyRedacted), []byte(secret)) {
		t.Fatal("test setup: dlp.Redact must have removed the secret")
	}
	msgs := []llm.Message{{Role: "user", Content: alreadyRedacted}}

	// With AlreadyRedacted=true in dlpResult, enqueueCapture passes needRedact=false.
	// The original findings are still in dlpRes.MsgFindings but captureBody must
	// not apply them again.
	body := captureBody(msgs, "ok", false /* needRedact=false, already redacted */, findings)

	if bytes.Contains(body, []byte(secret)) {
		t.Error("stored body must not contain raw secret")
	}
	n := bytes.Count(body, []byte("[REDACTED:"))
	if n != 1 {
		t.Errorf("expected exactly 1 [REDACTED:] marker (no double-redaction), got %d", n)
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
