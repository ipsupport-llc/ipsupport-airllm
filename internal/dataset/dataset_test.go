package dataset

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/rromenskyi/ipsupport-airllm/internal/capture"
	"github.com/rromenskyi/ipsupport-airllm/internal/dlp"
)

// --- fakes ---

type fakeStore struct {
	rows []capture.IndexRow
}

func (f *fakeStore) ListReviewed(_ context.Context) ([]capture.IndexRow, error) {
	return f.rows, nil
}

type fakeWriter struct {
	mu   sync.Mutex
	data map[string][]byte
}

func newFakeWriter() *fakeWriter {
	return &fakeWriter{data: map[string][]byte{}}
}

func (f *fakeWriter) Put(_ context.Context, key string, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make([]byte, len(data))
	copy(cp, data)
	f.data[key] = cp
	return nil
}

// --- helpers ---

// makeBody builds the JSON format that captureBody produces.
func makeBody(contents ...string) []byte {
	msgs := make([]map[string]any, len(contents))
	for i, c := range contents {
		msgs[i] = map[string]any{"role": "user", "content": c}
	}
	b, _ := json.Marshal(map[string]any{"messages": msgs, "response": "ok"})
	return b
}

// parseJSONL parses all JSON lines from JSONL bytes.
func parseJSONL(data []byte) ([]exportLine, error) {
	var lines []exportLine
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var l exportLine
		if err := json.Unmarshal(line, &l); err != nil {
			return nil, err
		}
		lines = append(lines, l)
	}
	return lines, sc.Err()
}

// --- tests ---

// TestExportEmitsCorrectJSONL verifies that a reviewed capture with detected
// findings produces a valid JSONL line with aligned text and spans.
func TestExportEmitsCorrectJSONL(t *testing.T) {
	msgText := "key: sk-test-1234567890abcdefghijk"
	finding := dlp.Finding{Label: "openai_key", Start: 5, End: len(msgText)}

	body := makeBody(msgText)
	store := &fakeStore{rows: []capture.IndexRow{
		{
			ID:           "c1",
			BlobKey:      "captures/c1",
			ReviewStatus: "confirmed",
			Detected:     []dlp.Finding{finding},
		},
	}}
	writer := newFakeWriter()
	readBody := func(_ context.Context, key string) ([]byte, error) {
		if key == "captures/c1" {
			return body, nil
		}
		return nil, errors.New("not found: " + key)
	}

	artifactKey, count, err := Export(context.Background(), store, readBody, writer)
	if err != nil {
		t.Fatalf("Export returned error: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 JSONL line, got %d", count)
	}

	artifact, ok := writer.data[artifactKey]
	if !ok {
		t.Fatalf("artifact key %q not found in writer", artifactKey)
	}

	lines, err := parseJSONL(artifact)
	if err != nil {
		t.Fatalf("parseJSONL: %v", err)
	}
	if len(lines) != 1 {
		t.Fatalf("expected 1 parsed line, got %d", len(lines))
	}

	got := lines[0]
	if got.Text != msgText {
		t.Errorf("text mismatch: got %q, want %q", got.Text, msgText)
	}
	if len(got.Spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(got.Spans))
	}
	s := got.Spans[0]
	if s.Label != "openai_key" {
		t.Errorf("label mismatch: got %q, want %q", s.Label, "openai_key")
	}
	if s.Start != finding.Start || s.End != finding.End {
		t.Errorf("span offsets: got [%d,%d], want [%d,%d]", s.Start, s.End, finding.Start, finding.End)
	}
	// Sanity-check offset alignment: the span must be within the text.
	if s.End > len(got.Text) {
		t.Errorf("span.End %d exceeds text length %d", s.End, len(got.Text))
	}
}

// TestExportPrefersRawBlob verifies that when an unexpired raw-window copy
// exists, Export reads it (aligned spans) rather than the redacted main body.
func TestExportPrefersRawBlob(t *testing.T) {
	raw := "key: sk-raw-1234567890abcdefghijk"
	finding := dlp.Finding{Label: "openai_key", Start: 5, End: len(raw)}
	future := time.Now().Add(time.Hour)

	store := &fakeStore{rows: []capture.IndexRow{
		{
			ID:           "c1",
			BlobKey:      "captures/c1",
			RawBlobKey:   "captures-raw/c1",
			RawExpiresAt: &future,
			ReviewStatus: "confirmed",
			Detected:     []dlp.Finding{finding},
		},
	}}
	writer := newFakeWriter()
	readBody := func(_ context.Context, key string) ([]byte, error) {
		switch key {
		case "captures-raw/c1":
			return makeBody(raw), nil
		case "captures/c1":
			// Redacted body is shorter; the original-offset span would not align.
			return makeBody("[REDACTED:openai_key]"), nil
		}
		return nil, errors.New("not found: " + key)
	}

	artifactKey, count, err := Export(context.Background(), store, readBody, writer)
	if err != nil {
		t.Fatalf("Export returned error: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 line from raw blob, got %d", count)
	}
	lines, err := parseJSONL(writer.data[artifactKey])
	if err != nil {
		t.Fatalf("parseJSONL: %v", err)
	}
	if len(lines) != 1 || lines[0].Text != raw {
		t.Fatalf("expected exported text from raw blob %q, got %+v", raw, lines)
	}
}

// TestExportPrefersGoldLabels verifies that gold_labels are used when non-empty,
// even when detected has different labels for the same span.
func TestExportPrefersGoldLabels(t *testing.T) {
	msgText := "key: sk-test-1234567890abcdefghijk"
	detected := dlp.Finding{Label: "high_entropy", Start: 5, End: 15}
	gold := dlp.Finding{Label: "openai_key", Start: 5, End: 15}

	store := &fakeStore{rows: []capture.IndexRow{
		{
			ID:           "g1",
			BlobKey:      "captures/g1",
			ReviewStatus: "confirmed",
			Detected:     []dlp.Finding{detected},
			GoldLabels:   []dlp.Finding{gold},
		},
	}}
	writer := newFakeWriter()
	readBody := func(_ context.Context, _ string) ([]byte, error) {
		return makeBody(msgText), nil
	}

	artifactKey, count, err := Export(context.Background(), store, readBody, writer)
	if err != nil {
		t.Fatalf("Export error: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 line, got %d", count)
	}

	lines, err := parseJSONL(writer.data[artifactKey])
	if err != nil {
		t.Fatalf("parseJSONL: %v", err)
	}
	if len(lines) == 0 {
		t.Fatal("no lines in artifact")
	}
	if lines[0].Spans[0].Label != "openai_key" {
		t.Errorf("expected gold label 'openai_key', got %q", lines[0].Spans[0].Label)
	}
}

// TestExportSkipsNoFindings verifies that captures with no findings (neither
// gold_labels nor detected) produce no JSONL output.
func TestExportSkipsNoFindings(t *testing.T) {
	store := &fakeStore{rows: []capture.IndexRow{
		{
			ID:           "nf1",
			BlobKey:      "captures/nf1",
			ReviewStatus: "confirmed",
			Detected:     nil,
			GoldLabels:   nil,
		},
	}}
	writer := newFakeWriter()
	readBody := func(_ context.Context, _ string) ([]byte, error) {
		return makeBody("clean text with no secrets"), nil
	}

	_, count, err := Export(context.Background(), store, readBody, writer)
	if err != nil {
		t.Fatalf("Export error: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 lines for no-findings capture, got %d", count)
	}
}

// TestExportMultiMessagePerCapture verifies that a capture with two messages
// produces two JSONL lines, each with correct per-message spans.
func TestExportMultiMessagePerCapture(t *testing.T) {
	msg0 := "key: sk-0000000000000000000000000001"
	msg1 := "token: sk-1111111111111111111111111002"
	// findings are per-message offsets
	f0 := dlp.Finding{Label: "openai_key", Start: 5, End: len(msg0)}
	f1 := dlp.Finding{Label: "openai_key", Start: 7, End: len(msg1)}

	store := &fakeStore{rows: []capture.IndexRow{
		{
			ID:           "mm1",
			BlobKey:      "captures/mm1",
			ReviewStatus: "false_negative",
			Detected:     []dlp.Finding{f0, f1},
		},
	}}
	writer := newFakeWriter()
	readBody := func(_ context.Context, _ string) ([]byte, error) {
		return makeBody(msg0, msg1), nil
	}

	artifactKey, count, err := Export(context.Background(), store, readBody, writer)
	if err != nil {
		t.Fatalf("Export error: %v", err)
	}
	if count != 2 {
		t.Fatalf("expected 2 JSONL lines (one per message), got %d", count)
	}

	lines, err := parseJSONL(writer.data[artifactKey])
	if err != nil {
		t.Fatalf("parseJSONL: %v", err)
	}
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d", len(lines))
	}
	if lines[0].Text != msg0 {
		t.Errorf("line 0 text mismatch: got %q, want %q", lines[0].Text, msg0)
	}
	if lines[1].Text != msg1 {
		t.Errorf("line 1 text mismatch: got %q, want %q", lines[1].Text, msg1)
	}
	// Each span must be within the bounds of its line's text.
	for i, l := range lines {
		for _, sp := range l.Spans {
			if sp.End > len(l.Text) {
				t.Errorf("line %d: span.End %d exceeds text length %d", i, sp.End, len(l.Text))
			}
		}
	}
}

// TestExportEmptyArtifactOnNoReviewed verifies that Export produces an empty
// artifact (but no error) when the store returns no rows.
func TestExportEmptyArtifactOnNoReviewed(t *testing.T) {
	store := &fakeStore{rows: nil}
	writer := newFakeWriter()
	readBody := func(_ context.Context, _ string) ([]byte, error) {
		return nil, errors.New("should not be called")
	}

	artifactKey, count, err := Export(context.Background(), store, readBody, writer)
	if err != nil {
		t.Fatalf("Export error: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 count, got %d", count)
	}
	if _, ok := writer.data[artifactKey]; !ok {
		t.Error("artifact blob must be written even when empty")
	}
}
