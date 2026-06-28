package httpapi

import (
	"bytes"
	"testing"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/dlp"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/llm"
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

// TestSnapshotOriginalsPreservesPreMaskContent guards the core of the raw-window
// fix: snapshotOriginals must capture message content BEFORE dlpEnforce redacts
// req.Messages in place, and only for action="redact". A later in-place
// reassignment of Content must not corrupt the snapshot.
func TestSnapshotOriginalsPreservesPreMaskContent(t *testing.T) {
	secret := "sk-ant-api03-aaaabbbbccccddddeeee1234"
	msgs := []llm.Message{{Role: "user", Content: "key: " + secret}}

	snap := snapshotOriginals("redact", msgs)
	if snap == nil {
		t.Fatal("redact action must snapshot originals")
	}
	// Simulate dlpEnforce's in-place redaction after the snapshot was taken.
	msgs[0].Content = "key: [REDACTED:anthropic_key]"
	if snap[0].Content != "key: "+secret {
		t.Fatalf("snapshot must preserve pre-mask content, got %q", snap[0].Content)
	}

	// Non-redact actions leave req.Messages untouched -> no snapshot needed.
	if snapshotOriginals("flag", msgs) != nil {
		t.Error("non-redact action must not snapshot (caller already holds originals)")
	}
	if snapshotOriginals("off", msgs) != nil {
		t.Error("off action must not snapshot")
	}
}

// TestRawBodyUsesOriginalsWhenDLPRedacted proves the raw-training window stores
// UN-redacted text even when DLP action="redact" masked req.Messages in place:
// the raw body is built from the originals preserved by dlpEnforce, while the
// main (durable) body stays redacted. This is the fix that makes the flywheel
// accurate on a redacted stream.
func TestRawBodyUsesOriginalsWhenDLPRedacted(t *testing.T) {
	secret := "sk-ant-api03-aaaabbbbccccddddeeee1234"
	content := "key: " + secret
	findings := [][]dlp.Finding{{{Label: "anthropic_key", Start: 5, End: 5 + len(secret)}}}

	// As dlpEnforce(action=redact) leaves things: originals preserved, msgs masked.
	original := []llm.Message{{Role: "user", Content: content}}
	masked := []llm.Message{{Role: "user", Content: dlp.Redact(content, findings[0])}}

	// Main body comes from the masked messages (no re-redaction).
	mainBody := captureBody(masked, "ok", false, findings)
	if bytes.Contains(mainBody, []byte(secret)) {
		t.Error("main body must stay redacted")
	}
	// Raw body comes from the preserved originals — un-redacted.
	rawBody := captureBody(original, "ok", false, nil)
	if !bytes.Contains(rawBody, []byte(secret)) {
		t.Error("raw body must contain the un-redacted secret (built from originals)")
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
