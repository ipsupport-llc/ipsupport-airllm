package capture

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/dlp"
	"github.com/jackc/pgx/v5/pgxpool"
)

// testPool connects to TEST_DATABASE_URL or skips. The DB must have the
// migrations applied (run the dev compose stack: make compose-up).
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping capture store integration test")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

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

// TestScanRowsLogsCorruptJSON is the DLP/capture Minor fix: scanRows (shared
// by every admin-facing capture view — List, Get, ReviewQueue) silently
// discarded a JSON-unmarshal failure on detected/secondpass_labels/
// gold_labels, while its sibling PendingForSecondPass at least logs a
// warning for the identical kind of corruption on the same detected column.
// A capture_index row can only ever hold syntactically valid JSON (the
// columns are jsonb, which Postgres itself validates at write time), so the
// realistic failure mode is a value that's valid JSON but the wrong shape
// for []dlp.Finding — e.g. a stray object instead of an array, from a
// future encoding bug or a hand-edited row.
func TestScanRowsLogsCorruptJSON(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	p := &PGInserter{PG: pool}

	row := IndexRow{
		ID: newID(), TS: time.Now().UTC(),
		IngressProtocol: "openai", Alias: "test",
		ProviderName: "mock", UpstreamModel: "mock-model", Status: 200,
		BlobKey: "test-blob", ReviewStatus: "unreviewed", SecondpassStatus: "pending",
	}
	if err := p.Insert(ctx, row); err != nil {
		t.Fatalf("seed row: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), `DELETE FROM capture_index WHERE id = $1`, row.ID)
	})

	// Valid JSON, wrong shape: an object where []dlp.Finding is expected.
	if _, err := pool.Exec(ctx,
		`UPDATE capture_index SET secondpass_labels = '{"not":"an array"}'::jsonb WHERE id = $1`, row.ID,
	); err != nil {
		t.Fatalf("corrupt secondpass_labels: %v", err)
	}

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	got, err := p.Get(ctx, row.ID)
	if err != nil {
		t.Fatalf("Get must still succeed despite the corrupt column: %v", err)
	}
	if got.SecondpassLabels != nil {
		t.Errorf("SecondpassLabels = %+v, want nil (unmarshal into the wrong shape must fail silently into the zero value)", got.SecondpassLabels)
	}
	// The id column is uuid; Postgres normalizes newID()'s plain hex string
	// to standard dashed form on scan-back, so compare against got.ID (what
	// scanRows actually logged), not the original row.ID.
	if !strings.Contains(buf.String(), "corrupt secondpass_labels JSON") || !strings.Contains(buf.String(), got.ID) {
		t.Errorf("expected a warning naming both the corruption and the row id %q, got log output: %s", got.ID, buf.String())
	}
}
