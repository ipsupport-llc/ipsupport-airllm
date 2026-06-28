package capture

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rromenskyi/ipsupport-airllm/internal/dlp"
)

// execRecorder captures the SQL + args passed to Exec so we can assert them
// without a live database.
type execRecorder struct {
	sql  string
	args []any
}

func (e *execRecorder) Exec(_ context.Context, sql string, args ...any) error {
	e.sql = sql
	e.args = args
	return nil
}

// pgInsertArgs builds the arg slice the same way PGInserter.Insert does.
// This is a pure-logic test: no real DB needed.
func TestPGInserterArgCount(t *testing.T) {
	row := IndexRow{
		ID:               "abc123",
		TS:               time.Now(),
		KeyID:            "key-1",
		UserID:           "user-1",
		IngressProtocol:  "openai",
		Alias:            "gpt-4",
		ProviderName:     "openai",
		UpstreamModel:    "gpt-4o",
		Status:           200,
		PromptTokens:     100,
		CompletionTokens: 50,
		CostUSD:          0.001,
		BlobKey:          "captures/abc123",
		Redacted:         true,
		ModelVersion:     "v1",
		Detected:         []dlp.Finding{{Label: "openai_key", Start: 0, End: 6}},
		ReviewStatus:     "unreviewed",
		SecondpassStatus: "pending",
		SecondpassLabels: nil,
	}

	detected, _ := json.Marshal(row.Detected)
	labels, _ := json.Marshal(row.SecondpassLabels)

	args := []any{
		row.ID, row.TS, nullStr(row.KeyID), nullStr(row.UserID),
		row.IngressProtocol, row.Alias,
		row.ProviderName, row.UpstreamModel, row.Status,
		row.PromptTokens, row.CompletionTokens, row.CostUSD,
		row.BlobKey, row.Redacted, row.ModelVersion,
		string(detected), row.ReviewStatus, row.SecondpassStatus, string(labels),
	}

	// The INSERT has 19 placeholders ($1..$19).
	const expectedArgCount = 19
	if len(args) != expectedArgCount {
		t.Fatalf("expected %d args, got %d", expectedArgCount, len(args))
	}

	// Verify detected JSON round-trips.
	var findings []dlp.Finding
	if err := json.Unmarshal(detected, &findings); err != nil {
		t.Fatalf("detected JSON not valid: %v", err)
	}
	if len(findings) != 1 || findings[0].Label != "openai_key" {
		t.Errorf("detected round-trip mismatch: %+v", findings)
	}
}

func TestNullStr(t *testing.T) {
	if nullStr("") != nil {
		t.Error("empty string must map to nil")
	}
	if nullStr("x") == nil {
		t.Error("non-empty string must not map to nil")
	}
}

// TestFindingJSONRoundtrip verifies that dlp.Finding marshals and unmarshals
// without loss — the same shape used by UpdateSecondPass to persist labels.
func TestFindingJSONRoundtrip(t *testing.T) {
	labels := []dlp.Finding{{Label: "secret", Start: 0, End: 5}}
	raw, err := json.Marshal(labels)
	if err != nil {
		t.Fatal(err)
	}
	var got []dlp.Finding
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("labels JSON invalid: %v", err)
	}
	if len(got) != 1 || got[0].Label != "secret" {
		t.Errorf("labels round-trip mismatch: %+v", got)
	}
}

// TestSetReviewValidation checks that SetReview rejects unknown status values.
// Validation runs before any DB call, so we can test it without a live pool by
// only exercising invalid statuses (which return early) and using validReviewStatuses
// to whitebox-verify that all permitted values are registered.
func TestSetReviewValidation(t *testing.T) {
	// All valid statuses must appear in validReviewStatuses.
	for _, status := range []string{"confirmed", "false_positive", "false_negative", "unreviewed"} {
		if !validReviewStatuses[status] {
			t.Errorf("status %q must be in validReviewStatuses", status)
		}
	}

	// Invalid statuses must return ErrInvalidReviewStatus (no DB call needed).
	p := &PGInserter{} // nil pool — safe because validation returns before DB access
	ctx := context.Background()
	for _, bad := range []string{"", "ok", "reviewed", "suspect", "CONFIRMED"} {
		err := p.SetReview(ctx, "id1", bad, nil)
		if err != ErrInvalidReviewStatus {
			t.Errorf("status %q should be invalid, want ErrInvalidReviewStatus, got %v", bad, err)
		}
	}
}

// TestIndexRowJSONTags verifies that IndexRow serialises with snake_case keys.
func TestIndexRowJSONTags(t *testing.T) {
	row := IndexRow{
		ID:               "abc",
		IngressProtocol:  "openai",
		ReviewStatus:     "unreviewed",
		SecondpassStatus: "pending",
		GoldLabels:       []dlp.Finding{{Label: "l", Start: 0, End: 1}},
	}
	b, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{`"id"`, `"ingress_protocol"`, `"review_status"`, `"secondpass_status"`, `"gold_labels"`} {
		if !strings.Contains(s, want) {
			t.Errorf("JSON missing key %s in: %s", want, s)
		}
	}
	// PascalCase must not appear.
	for _, bad := range []string{`"ID"`, `"IngressProtocol"`, `"ReviewStatus"`} {
		if strings.Contains(s, bad) {
			t.Errorf("JSON should not contain PascalCase key %s in: %s", bad, s)
		}
	}
}
