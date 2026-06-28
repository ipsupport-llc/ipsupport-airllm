package secondpass

import (
	"context"
	"testing"

	"github.com/rromenskyi/ipsupport-airllm/internal/dlp"
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
	got := parseFindings(raw, 0.5)
	if len(got) != 1 || got[0].Label != "openai_key" || got[0].Start != 0 || got[0].End != 10 {
		t.Fatalf("unexpected findings: %v", got)
	}
}

func TestParseFindings_BelowMinScore(t *testing.T) {
	raw := `[{"label":"key","start":0,"end":5,"score":0.3}]`
	got := parseFindings(raw, 0.5)
	if len(got) != 0 {
		t.Fatalf("expected 0 findings below min score, got %v", got)
	}
}

func TestParseFindings_MultipleScores(t *testing.T) {
	raw := `[
		{"label":"a","start":0,"end":5,"score":0.9},
		{"label":"b","start":10,"end":15,"score":0.1}
	]`
	got := parseFindings(raw, 0.5)
	if len(got) != 1 || got[0].Label != "a" {
		t.Fatalf("expected only high-score finding, got %v", got)
	}
}

func TestParseFindings_Malformed(t *testing.T) {
	got := parseFindings("not-json{{{", 0.5)
	if got != nil {
		t.Fatalf("expected nil for malformed JSON, got %v", got)
	}
}

func TestParseFindings_EmptyArray(t *testing.T) {
	got := parseFindings("[]", 0.5)
	if len(got) != 0 {
		t.Fatalf("expected empty slice, got %v", got)
	}
}

func TestParseFindings_DegenerateSpan(t *testing.T) {
	// end <= start must be dropped.
	raw := `[{"label":"key","start":5,"end":5,"score":0.9}]`
	got := parseFindings(raw, 0.0)
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

func TestLLMEngine_Scan_MalformedOutputNoError(t *testing.T) {
	e := &LLMEngine{
		Chat: func(_ context.Context, _ string) (string, error) {
			return "Sorry, I found a key at position 5.", nil
		},
		MinScore: func() float64 { return 0.5 },
	}
	got, err := e.Scan(context.Background(), "text")
	if err != nil {
		t.Fatal(err)
	}
	// Malformed output -> no findings, no crash.
	if got != nil {
		t.Fatalf("expected nil findings for malformed LLM output, got %v", got)
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
	job := NewJob(store, fakeReadBody([]byte("hello")), engine, nil, 10)
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
	job := NewJob(store, fakeReadBody([]byte("hello")), engine, nil, 10)
	job.RunOnce(context.Background())

	u, ok := store.updates["id2"]
	if !ok {
		t.Fatal("expected update for id2")
	}
	if u.status != "false_positive" {
		t.Fatalf("expected false_positive, got %s", u.status)
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

	job := NewJob(store, fakeReadBody([]byte("hello")), engine, hook, 10)
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

	job := NewJob(store, fakeReadBody([]byte("hello")), engine, hook, 10)
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
	job := NewJob(store, fakeReadBody([]byte("hello")), engine, nil, 10)
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

	job := NewJob(store, fakeReadBody([]byte("[REDACTED:key]")), engine, hook, 10)
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

func TestJob_MalformedEngineOutput_NoCrash(t *testing.T) {
	store := newFakeStore(PendingRow{
		ID:      "id5",
		BlobKey: "k5",
	})
	// LLMEngine returning malformed JSON: no crash, no findings.
	engine := &LLMEngine{
		Chat:     func(_ context.Context, _ string) (string, error) { return "not-json!", nil },
		MinScore: func() float64 { return 0.5 },
	}
	job := NewJob(store, fakeReadBody([]byte("hello")), engine, nil, 10)
	job.RunOnce(context.Background()) // must not panic

	// Malformed LLM output -> no findings -> clean (no detected either)
	u, ok := store.updates["id5"]
	if !ok {
		t.Fatal("expected update for id5")
	}
	if u.status != "clean" {
		t.Fatalf("expected clean for malformed LLM output with no detected, got %s", u.status)
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
	job := NewJob(store, errBody, engine, nil, 10)
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
	job := NewJob(store, fakeReadBody([]byte("hello")), engine, nil, 10)
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
