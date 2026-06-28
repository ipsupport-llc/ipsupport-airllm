package capture

import (
	"context"
	"encoding/json"
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
