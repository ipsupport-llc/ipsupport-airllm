package secondpass

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/dlp"
)

// --- fakeEngine ---

type fakeEngine struct {
	findings []dlp.Finding
	err      error
}

func (f *fakeEngine) Scan(_ context.Context, _ string) ([]dlp.Finding, error) {
	return f.findings, f.err
}

// --- fakeStore ---

type update struct {
	status string
	labels []dlp.Finding
}

type fakeStore struct {
	pending []PendingRow
	updates map[string]update
}

func newFakeStore(pending ...PendingRow) *fakeStore {
	return &fakeStore{pending: pending, updates: make(map[string]update)}
}

func (f *fakeStore) PendingForSecondPass(_ context.Context, limit int) ([]PendingRow, error) {
	if limit > 0 && limit < len(f.pending) {
		return f.pending[:limit], nil
	}
	return f.pending, nil
}

func (f *fakeStore) UpdateSecondPass(_ context.Context, id, status string, labels []dlp.Finding) error {
	f.updates[id] = update{status: status, labels: labels}
	return nil
}

// --- parseFindings tests ---

func TestParseFindings_Valid(t *testing.T) {
	raw := `[{"label":"openai_key","start":0,"end":10,"score":0.9}]`
	got, err := parseFindings(raw, 0.5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Label != "openai_key" || got[0].Start != 0 || got[0].End != 10 {
		t.Fatalf("unexpected findings: %v", got)
	}
}

func TestParseFindings_BelowMinScore(t *testing.T) {
	raw := `[{"label":"key","start":0,"end":5,"score":0.3}]`
	got, err := parseFindings(raw, 0.5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("expected 0 findings below min score, got %v", got)
	}
}

func TestParseFindings_MultipleScores(t *testing.T) {
	raw := `[
		{"label":"a","start":0,"end":5,"score":0.9},
		{"label":"b","start":10,"end":15,"score":0.1}
	]`
	got, err := parseFindings(raw, 0.5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Label != "a" {
		t.Fatalf("expected only high-score finding, got %v", got)
	}
}

// TestParseFindings_Malformed is the DLP/secondpass Minor fix: malformed
// LLM output must be a real error, not a silent "zero findings" — the
// caller (LLMEngine.Scan, then processOne) needs to tell "the engine
// scanned and confirmed nothing" apart from "the engine's output couldn't
// even be parsed", since the two lead to very different, safe-vs-unsafe
// outcomes for an already-detected secret.
func TestParseFindings_Malformed(t *testing.T) {
	got, err := parseFindings("not-json{{{", 0.5)
	if err == nil {
		t.Fatal("expected an error for malformed JSON, got nil")
	}
	if got != nil {
		t.Fatalf("expected nil findings alongside the error, got %v", got)
	}
}

func TestParseFindings_EmptyArray(t *testing.T) {
	got, err := parseFindings("[]", 0.5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty slice, got %v", got)
	}
}

func TestParseFindings_DegenerateSpan(t *testing.T) {
	// end <= start must be dropped.
	raw := `[{"label":"key","start":5,"end":5,"score":0.9}]`
	got, err := parseFindings(raw, 0.0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("expected 0 for degenerate span, got %v", got)
	}
}

// --- LLMEngine.Scan tests ---

func TestLLMEngine_Scan_Valid(t *testing.T) {
	e := &LLMEngine{
		Chat: func(_ context.Context, _ string) (string, error) {
			return `[{"label":"api_key","start":5,"end":15,"score":0.95}]`, nil
		},
		MinScore: func() float64 { return 0.5 },
	}
	got, err := e.Scan(context.Background(), "test text")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Label != "api_key" {
		t.Fatalf("unexpected findings: %v", got)
	}
}

// TestLLMEngine_Scan_MalformedOutputReturnsError is the DLP/secondpass
// Minor fix: a real-world LLM response that ignores "no prose, no markdown
// fences" must surface as a scan error, not succeed with zero findings. A
// zero-findings success on an already-detected secret gets treated by
// processOne as "the stronger engine looked and found nothing" — which (for
// an unredacted or raw-window body) clears the alert as a false positive.
func TestLLMEngine_Scan_MalformedOutputReturnsError(t *testing.T) {
	e := &LLMEngine{
		Chat: func(_ context.Context, _ string) (string, error) {
			return "Sorry, I found a key at position 5.", nil
		},
		MinScore: func() float64 { return 0.5 },
	}
	got, err := e.Scan(context.Background(), "text")
	if err == nil {
		t.Fatal("expected an error for malformed LLM output, got nil")
	}
	if got != nil {
		t.Fatalf("expected nil findings alongside the error, got %v", got)
	}
}

// --- diff tests ---

func TestDiff_Clean(t *testing.T) {
	status, fps, misses := diff(nil, nil)
	if status != "clean" || len(fps) != 0 || len(misses) != 0 {
		t.Fatalf("clean: got status=%s fps=%v misses=%v", status, fps, misses)
	}
}

func TestDiff_Confirmed(t *testing.T) {
	d := []dlp.Finding{{Label: "key", Start: 0, End: 10}}
	e := []dlp.Finding{{Label: "key", Start: 0, End: 10}}
	status, fps, misses := diff(d, e)
	if status != "confirmed" || len(fps) != 0 || len(misses) != 0 {
		t.Fatalf("confirmed: got status=%s fps=%v misses=%v", status, fps, misses)
	}
}

func TestDiff_FalsePositive(t *testing.T) {
	d := []dlp.Finding{{Label: "key", Start: 0, End: 10}}
	status, fps, misses := diff(d, nil)
	if status != "false_positive" || len(fps) != 1 || len(misses) != 0 {
		t.Fatalf("fp: got status=%s fps=%v misses=%v", status, fps, misses)
	}
}

func TestDiff_FalseNegative(t *testing.T) {
	e := []dlp.Finding{{Label: "secret", Start: 5, End: 15}}
	status, fps, misses := diff(nil, e)
	if status != "false_negative" || len(fps) != 0 || len(misses) != 1 {
		t.Fatalf("fn: got status=%s fps=%v misses=%v", status, fps, misses)
	}
}

func TestDiff_BothFPandFN_PreferFN(t *testing.T) {
	detected := []dlp.Finding{{Label: "fp-only", Start: 100, End: 110}}
	engine := []dlp.Finding{{Label: "fn-only", Start: 0, End: 10}}
	status, fps, misses := diff(detected, engine)
	if status != "false_negative" {
		t.Fatalf("both FP+FN must prefer false_negative, got %s", status)
	}
	if len(fps) != 1 || len(misses) != 1 {
		t.Fatalf("fps=%v misses=%v", fps, misses)
	}
}

func TestDiff_OverlapConfirms(t *testing.T) {
	// Engine span overlaps but doesn't match exactly: still confirmed.
	// detected=[0,20), engine=[5,15): mutual overlap → confirmed (not clean,
	// not FP, not FN). "confirmed" is the only reachable status here.
	d := []dlp.Finding{{Label: "key", Start: 0, End: 20}}
	e := []dlp.Finding{{Label: "key", Start: 5, End: 15}} // subset
	status, _, _ := diff(d, e)
	if status != "confirmed" {
		t.Fatalf("overlap: expected confirmed, got %s", status)
	}
}

// --- Job.RunOnce tests ---

func TestJob_ConfirmPath(t *testing.T) {
	store := newFakeStore(PendingRow{
		ID:       "id1",
		BlobKey:  "k1",
		Detected: []dlp.Finding{{Label: "key", Start: 0, End: 5}},
	})
	engine := &fakeEngine{findings: []dlp.Finding{{Label: "key", Start: 0, End: 5}}}
	job := NewJob(store, fakeReadBody([]byte("hello")), engine, nil, 10, func() bool { return false })
	job.RunOnce(context.Background())

	u, ok := store.updates["id1"]
	if !ok {
		t.Fatal("expected update for id1")
	}
	if u.status != "confirmed" {
		t.Fatalf("expected confirmed, got %s", u.status)
	}
}

func TestJob_FalsePositivePath(t *testing.T) {
	store := newFakeStore(PendingRow{
		ID:       "id2",
		BlobKey:  "k2",
		Detected: []dlp.Finding{{Label: "key", Start: 0, End: 5}},
	})
	engine := &fakeEngine{findings: nil} // engine finds nothing -> FP
	job := NewJob(store, fakeReadBody([]byte("hello")), engine, nil, 10, func() bool { return false })
	job.RunOnce(context.Background())

	u, ok := store.updates["id2"]
	if !ok {
		t.Fatal("expected update for id2")
	}
	if u.status != "false_positive" {
		t.Fatalf("expected false_positive, got %s", u.status)
	}
}

// TestJob_EngineScanErrorLeavesRowPending is the DLP/secondpass Minor fix,
// proven end to end through the real Job.RunOnce/processOne path: when the
// engine returns an error (standing in for LLMEngine.Scan's malformed-output
// case), the row must be left untouched for retry, never updated — in
// particular never marked false_positive, which is what happened before the
// fix when a parse failure was silently treated as "zero findings".
func TestJob_EngineScanErrorLeavesRowPending(t *testing.T) {
	store := newFakeStore(PendingRow{
		ID:       "id-scan-err",
		BlobKey:  "k-scan-err",
		Redacted: false, // unredacted: the false-positive downgrade safety net does NOT apply here
		Detected: []dlp.Finding{{Label: "openai_key", Start: 0, End: 5}},
	})
	engine := &fakeEngine{err: errors.New("secondpass: malformed LLM output: unexpected end of JSON input")}
	job := NewJob(store, fakeReadBody([]byte("hello")), engine, nil, 10, func() bool { return false })
	job.RunOnce(context.Background())

	if _, ok := store.updates["id-scan-err"]; ok {
		t.Fatalf("row was updated despite a scan error; want it left pending for retry: %+v", store.updates["id-scan-err"])
	}
}

func TestJob_FalseNegativePath(t *testing.T) {
	store := newFakeStore(PendingRow{
		ID:       "id3",
		BlobKey:  "k3",
		Detected: nil, // no initial detections
	})
	miss := dlp.Finding{Label: "secret", Start: 0, End: 10}
	engine := &fakeEngine{findings: []dlp.Finding{miss}}

	var hookEvent string
	hook := WebhookSender(func(_ context.Context, event string, _ []byte) {
		hookEvent = event
	})

	job := NewJob(store, fakeReadBody([]byte("hello")), engine, hook, 10, func() bool { return false })
	job.RunOnce(context.Background())

	u, ok := store.updates["id3"]
	if !ok {
		t.Fatal("expected update for id3")
	}
	if u.status != "false_negative" {
		t.Fatalf("expected false_negative, got %s", u.status)
	}
	if hookEvent != "dlp.false_negative" {
		t.Fatalf("expected dlp.false_negative webhook, got %q", hookEvent)
	}
}

func TestJob_FalsePositiveFiresAlertClearedWebhook(t *testing.T) {
	store := newFakeStore(PendingRow{
		ID:       "id-fp",
		BlobKey:  "k-fp",
		Detected: []dlp.Finding{{Label: "key", Start: 0, End: 5}},
	})
	engine := &fakeEngine{findings: nil}

	var hookEvent string
	hook := WebhookSender(func(_ context.Context, event string, _ []byte) {
		hookEvent = event
	})

	job := NewJob(store, fakeReadBody([]byte("hello")), engine, hook, 10, func() bool { return false })
	job.RunOnce(context.Background())

	if hookEvent != "dlp.alert_cleared" {
		t.Fatalf("expected dlp.alert_cleared webhook for FP, got %q", hookEvent)
	}
}

func TestJob_CleanPath(t *testing.T) {
	store := newFakeStore(PendingRow{
		ID:      "id4",
		BlobKey: "k4",
	})
	engine := &fakeEngine{findings: nil}
	job := NewJob(store, fakeReadBody([]byte("hello")), engine, nil, 10, func() bool { return false })
	job.RunOnce(context.Background())

	u, ok := store.updates["id4"]
	if !ok {
		t.Fatal("expected update for id4")
	}
	if u.status != "clean" {
		t.Fatalf("expected clean, got %s", u.status)
	}
}

// TestJob_RedactedRow_NoFalsePositive verifies that when a capture was stored
// with Redacted=true the second-pass never emits false_positive or fires the
// dlp.alert_cleared webhook, even when the engine finds nothing (because the
// secret is masked). The detection is treated as confirmed instead.
func TestJob_RedactedRow_NoFalsePositive(t *testing.T) {
	store := newFakeStore(PendingRow{
		ID:       "id-red",
		BlobKey:  "k-red",
		Detected: []dlp.Finding{{Label: "key", Start: 0, End: 5}},
		Redacted: true,
	})
	engine := &fakeEngine{findings: nil} // engine sees "[REDACTED:key]", finds nothing

	var hookEvent string
	hook := WebhookSender(func(_ context.Context, event string, _ []byte) {
		hookEvent = event
	})

	job := NewJob(store, fakeReadBody([]byte("[REDACTED:key]")), engine, hook, 10, func() bool { return false })
	job.RunOnce(context.Background())

	u, ok := store.updates["id-red"]
	if !ok {
		t.Fatal("expected update for id-red")
	}
	if u.status == "false_positive" {
		t.Fatal("redacted row must not be classified as false_positive")
	}
	if u.status != "confirmed" {
		t.Fatalf("redacted row with detections must be confirmed, got %s", u.status)
	}
	if hookEvent == "dlp.alert_cleared" {
		t.Fatal("dlp.alert_cleared must not fire for redacted rows")
	}
}

// TestJob_RawWindow_AllowsFalsePositive verifies that when an un-redacted
// raw-window copy is available (and unexpired), the second-pass scans aligned
// text and may legitimately clear a false positive — the redacted-row downgrade
// is skipped.
func TestJob_RawWindow_AllowsFalsePositive(t *testing.T) {
	future := time.Now().Add(time.Hour)
	store := newFakeStore(PendingRow{
		ID:           "id-raw",
		BlobKey:      "k-red",
		Detected:     []dlp.Finding{{Label: "key", Start: 0, End: 5}},
		Redacted:     true,
		RawBlobKey:   "k-raw",
		RawExpiresAt: &future,
	})
	engine := &fakeEngine{findings: nil} // raw text is benign -> detection is a true FP

	var hookEvent string
	hook := WebhookSender(func(_ context.Context, event string, _ []byte) { hookEvent = event })

	job := NewJob(store, fakeReadBody([]byte("totally benign text")), engine, hook, 10, func() bool { return true })
	job.RunOnce(context.Background())

	u, ok := store.updates["id-raw"]
	if !ok {
		t.Fatal("expected update for id-raw")
	}
	if u.status != "false_positive" {
		t.Fatalf("raw-window row must allow false_positive, got %s", u.status)
	}
	if hookEvent != "dlp.alert_cleared" {
		t.Fatalf("expected dlp.alert_cleared, got %q", hookEvent)
	}
}

// TestJob_ExpiredRawWindow_StillDowngrades verifies that an expired raw copy is
// ignored: the redacted main body is scanned and the FP downgrade still applies.
func TestJob_ExpiredRawWindow_StillDowngrades(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	store := newFakeStore(PendingRow{
		ID:           "id-exp",
		BlobKey:      "k-red",
		Detected:     []dlp.Finding{{Label: "key", Start: 0, End: 5}},
		Redacted:     true,
		RawBlobKey:   "k-raw",
		RawExpiresAt: &past,
	})
	engine := &fakeEngine{findings: nil}

	job := NewJob(store, fakeReadBody([]byte("[REDACTED:key]")), engine, nil, 10, func() bool { return true })
	job.RunOnce(context.Background())

	u, ok := store.updates["id-exp"]
	if !ok {
		t.Fatal("expected update for id-exp")
	}
	if u.status != "confirmed" {
		t.Fatalf("expired raw must fall back to redacted downgrade (confirmed), got %s", u.status)
	}
}

// TestJob_RawDisallowedByDefault_FallsBackToRedactedBody proves that a
// valid, unexpired raw window is NOT used when allowRaw returns false (DLP
// C4 fix) — the row behaves exactly as if no raw window existed at all, so
// the un-redacted copy is never sent to the (possibly third-party) second-
// pass model alias without an explicit, separate opt-in.
func TestJob_RawDisallowedByDefault_FallsBackToRedactedBody(t *testing.T) {
	future := time.Now().Add(time.Hour)
	store := newFakeStore(PendingRow{
		ID:           "id-raw-disallowed",
		BlobKey:      "k-red",
		Detected:     []dlp.Finding{{Label: "key", Start: 0, End: 5}},
		Redacted:     true,
		RawBlobKey:   "k-raw",
		RawExpiresAt: &future,
	})
	engine := &fakeEngine{findings: nil} // would look like a true FP if raw were used

	job := NewJob(store, fakeReadBody([]byte("[REDACTED:key]")), engine, nil, 10, func() bool { return false })
	job.RunOnce(context.Background())

	u, ok := store.updates["id-raw-disallowed"]
	if !ok {
		t.Fatal("expected update for id-raw-disallowed")
	}
	if u.status != "confirmed" {
		t.Fatalf("raw window must be ignored when allowRaw=false, so the redacted downgrade still applies; got %s", u.status)
	}
}

// TestJob_MalformedEngineOutput_LeavesRowPending is the DLP/secondpass
// Minor fix, through the real LLMEngine (not a fake): malformed LLM output
// must not crash, but it also must not be treated as a successful
// zero-findings scan — it's a scan error, so the row is left pending for
// retry rather than updated at all.
func TestJob_MalformedEngineOutput_LeavesRowPending(t *testing.T) {
	store := newFakeStore(PendingRow{
		ID:      "id5",
		BlobKey: "k5",
	})
	engine := &LLMEngine{
		Chat:     func(_ context.Context, _ string) (string, error) { return "not-json!", nil },
		MinScore: func() float64 { return 0.5 },
	}
	job := NewJob(store, fakeReadBody([]byte("hello")), engine, nil, 10, func() bool { return false })
	job.RunOnce(context.Background()) // must not panic

	if u, ok := store.updates["id5"]; ok {
		t.Fatalf("row was updated despite malformed LLM output; want it left pending for retry: %+v", u)
	}
}

func TestJob_ReadBodyError_LeavePending(t *testing.T) {
	store := newFakeStore(PendingRow{
		ID:      "id6",
		BlobKey: "k6",
	})
	engine := &fakeEngine{findings: nil}
	errBody := func(_ context.Context, _ string) ([]byte, error) {
		return nil, errReadBody
	}
	job := NewJob(store, errBody, engine, nil, 10, func() bool { return false })
	job.RunOnce(context.Background())

	// Body read error -> row left pending (no update).
	if _, ok := store.updates["id6"]; ok {
		t.Fatal("row should remain pending when body read fails")
	}
}

func TestJob_EngineScanError_LeavePending(t *testing.T) {
	store := newFakeStore(PendingRow{
		ID:      "id-scan-err",
		BlobKey: "k-scan-err",
	})
	engine := &fakeEngine{err: errStr("scan failed")}
	job := NewJob(store, fakeReadBody([]byte("hello")), engine, nil, 10, func() bool { return false })
	job.RunOnce(context.Background())

	// Engine error -> row left pending (no update).
	if _, ok := store.updates["id-scan-err"]; ok {
		t.Fatal("row should remain pending when engine scan fails")
	}
}

// helpers

var errReadBody = errStr("read body failed")

type errStr string

func (e errStr) Error() string { return string(e) }

func fakeReadBody(data []byte) func(context.Context, string) ([]byte, error) {
	return func(_ context.Context, _ string) ([]byte, error) { return data, nil }
}
