package capture

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rromenskyi/ipsupport-airllm/internal/dlp"
)

// IndexRow mirrors the capture_index table.
type IndexRow struct {
	ID               string        `json:"id"`
	TS               time.Time     `json:"ts"`
	KeyID            string        `json:"key_id"`
	UserID           string        `json:"user_id"`
	IngressProtocol  string        `json:"ingress_protocol"`
	Alias            string        `json:"alias"`
	ProviderName     string        `json:"provider_name"`
	UpstreamModel    string        `json:"upstream_model"`
	Status           int           `json:"status"`
	PromptTokens     int64         `json:"prompt_tokens"`
	CompletionTokens int64         `json:"completion_tokens"`
	CostUSD          float64       `json:"cost_usd"`
	BlobKey          string        `json:"blob_key"`
	Redacted         bool          `json:"redacted"`
	ModelVersion     string        `json:"model_version"`
	Detected         []dlp.Finding `json:"detected"`
	ReviewStatus     string        `json:"review_status"`
	SecondpassStatus string        `json:"secondpass_status"`
	SecondpassLabels []dlp.Finding `json:"secondpass_labels"`
	GoldLabels       []dlp.Finding `json:"gold_labels"`
}

// newID returns a hex-encoded random 16-byte ID.
func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// PGInserter implements Inserter against a live pgxpool.Pool.
type PGInserter struct {
	PG *pgxpool.Pool
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Insert writes one row to capture_index.
func (p *PGInserter) Insert(ctx context.Context, row IndexRow) error {
	detected, err := json.Marshal(row.Detected)
	if err != nil {
		detected = []byte("[]")
	}
	labels, err := json.Marshal(row.SecondpassLabels)
	if err != nil {
		labels = []byte("[]")
	}
	_, err = p.PG.Exec(ctx, `
		INSERT INTO capture_index (
			id, ts, key_id, user_id, ingress_protocol, alias,
			provider_name, upstream_model, status,
			prompt_tokens, completion_tokens, cost_usd,
			blob_key, redacted, model_version,
			detected, review_status, secondpass_status, secondpass_labels
		) VALUES (
			$1, $2, $3, $4, $5, $6,
			$7, $8, $9,
			$10, $11, $12,
			$13, $14, $15,
			$16, $17, $18, $19
		)`,
		row.ID, row.TS, nullStr(row.KeyID), nullStr(row.UserID),
		row.IngressProtocol, row.Alias,
		row.ProviderName, row.UpstreamModel, row.Status,
		row.PromptTokens, row.CompletionTokens, row.CostUSD,
		row.BlobKey, row.Redacted, row.ModelVersion,
		string(detected), row.ReviewStatus, row.SecondpassStatus, string(labels),
	)
	return err
}

// ListExpired returns rows whose ts is before the cutoff time.
func (p *PGInserter) ListExpired(ctx context.Context, before time.Time) ([]IndexRow, error) {
	rows, err := p.PG.Query(ctx,
		`SELECT id, ts, blob_key FROM capture_index WHERE ts < $1`, before)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []IndexRow
	for rows.Next() {
		var r IndexRow
		if err := rows.Scan(&r.ID, &r.TS, &r.BlobKey); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeleteByID removes a capture_index row by id.
func (p *PGInserter) DeleteByID(ctx context.Context, id string) error {
	_, err := p.PG.Exec(ctx, `DELETE FROM capture_index WHERE id = $1`, id)
	return err
}

// ListFilter carries search parameters for List.
type ListFilter struct {
	ReviewStatus     string
	SecondpassStatus string
	From             *time.Time // inclusive lower bound on ts; nil = no lower bound
	To               *time.Time // inclusive upper bound on ts; nil = no upper bound
	Limit            int
}

// List searches capture_index with optional filters.
func (p *PGInserter) List(ctx context.Context, f ListFilter) ([]IndexRow, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	rows, err := p.PG.Query(ctx, `
		SELECT id, ts, key_id, user_id, ingress_protocol, alias,
		       provider_name, upstream_model, status,
		       prompt_tokens, completion_tokens, cost_usd,
		       blob_key, redacted, model_version,
		       detected, review_status, secondpass_status, secondpass_labels, gold_labels
		FROM capture_index
		WHERE ($1 = '' OR review_status = $1)
		  AND ($2 = '' OR secondpass_status = $2)
		  AND ($3::timestamptz IS NULL OR ts >= $3::timestamptz)
		  AND ($4::timestamptz IS NULL OR ts <= $4::timestamptz)
		ORDER BY ts DESC
		LIMIT $5`,
		f.ReviewStatus, f.SecondpassStatus, f.From, f.To, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRows(rows)
}

// Get returns one capture_index row by id.
func (p *PGInserter) Get(ctx context.Context, id string) (IndexRow, error) {
	rows, err := p.PG.Query(ctx, `
		SELECT id, ts, key_id, user_id, ingress_protocol, alias,
		       provider_name, upstream_model, status,
		       prompt_tokens, completion_tokens, cost_usd,
		       blob_key, redacted, model_version,
		       detected, review_status, secondpass_status, secondpass_labels, gold_labels
		FROM capture_index WHERE id = $1`, id)
	if err != nil {
		return IndexRow{}, err
	}
	defer rows.Close()
	out, err := scanRows(rows)
	if err != nil {
		return IndexRow{}, err
	}
	if len(out) == 0 {
		return IndexRow{}, ErrNotFound
	}
	return out[0], nil
}

// notFoundError is the type for ErrNotFound.
type notFoundError struct{}

func (notFoundError) Error() string { return "capture: not found" }

// ErrNotFound is returned by Get when no row matches.
var ErrNotFound = notFoundError{}

// ErrInvalidReviewStatus is returned by SetReview when reviewStatus is not one
// of the allowed values.
var ErrInvalidReviewStatus = errInvalidReviewStatus{}

type errInvalidReviewStatus struct{}

func (errInvalidReviewStatus) Error() string {
	return "capture: review_status must be one of: confirmed, false_positive, false_negative, unreviewed"
}

// validReviewStatuses is the set of permitted values for review_status.
var validReviewStatuses = map[string]bool{
	"confirmed":      true,
	"false_positive": true,
	"false_negative": true,
	"unreviewed":     true,
}

// ReviewQueue returns captures pending review: rows where review_status is
// 'unreviewed' or secondpass_status is 'suspect', newest first.
func (p *PGInserter) ReviewQueue(ctx context.Context, limit int) ([]IndexRow, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := p.PG.Query(ctx, `
		SELECT id, ts, key_id, user_id, ingress_protocol, alias,
		       provider_name, upstream_model, status,
		       prompt_tokens, completion_tokens, cost_usd,
		       blob_key, redacted, model_version,
		       detected, review_status, secondpass_status, secondpass_labels, gold_labels
		FROM capture_index
		WHERE review_status = 'unreviewed' OR secondpass_status = 'suspect'
		ORDER BY ts DESC
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRows(rows)
}

// PendingForSecondPass returns captures whose secondpass_status is 'pending',
// ordered oldest-first, up to limit rows. limit <= 0 defaults to 50.
func (p *PGInserter) PendingForSecondPass(ctx context.Context, limit int) ([]IndexRow, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := p.PG.Query(ctx, `
		SELECT id, blob_key, detected
		FROM capture_index
		WHERE secondpass_status = 'pending'
		ORDER BY ts ASC
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []IndexRow
	for rows.Next() {
		var r IndexRow
		var detectedRaw []byte
		if err := rows.Scan(&r.ID, &r.BlobKey, &detectedRaw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(detectedRaw, &r.Detected); err != nil {
			slog.Warn("capture: corrupt detected JSON", "id", r.ID, "err", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UpdateSecondPass sets secondpass_status and secondpass_labels for the given
// capture row.
func (p *PGInserter) UpdateSecondPass(ctx context.Context, id, status string, labels []dlp.Finding) error {
	raw, err := json.Marshal(labels)
	if err != nil {
		raw = []byte("[]")
	}
	_, err = p.PG.Exec(ctx, `
		UPDATE capture_index SET secondpass_status = $1, secondpass_labels = $2 WHERE id = $3`,
		status, string(raw), id)
	return err
}

// SetReview updates review_status and gold_labels for the given capture.
// reviewStatus must be one of: confirmed, false_positive, false_negative, unreviewed.
func (p *PGInserter) SetReview(ctx context.Context, id string, reviewStatus string, goldLabels []dlp.Finding) error {
	if !validReviewStatuses[reviewStatus] {
		return ErrInvalidReviewStatus
	}
	gold, err := json.Marshal(goldLabels)
	if err != nil {
		gold = []byte("[]")
	}
	_, err = p.PG.Exec(ctx, `
		UPDATE capture_index SET review_status = $1, gold_labels = $2 WHERE id = $3`,
		reviewStatus, string(gold), id)
	return err
}

func scanRows(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
}) ([]IndexRow, error) {
	var out []IndexRow
	for rows.Next() {
		var r IndexRow
		var keyID, userID *string
		var detectedRaw, secondpassRaw, goldRaw []byte
		if err := rows.Scan(
			&r.ID, &r.TS, &keyID, &userID,
			&r.IngressProtocol, &r.Alias,
			&r.ProviderName, &r.UpstreamModel, &r.Status,
			&r.PromptTokens, &r.CompletionTokens, &r.CostUSD,
			&r.BlobKey, &r.Redacted, &r.ModelVersion,
			&detectedRaw, &r.ReviewStatus, &r.SecondpassStatus, &secondpassRaw, &goldRaw,
		); err != nil {
			return nil, err
		}
		if keyID != nil {
			r.KeyID = *keyID
		}
		if userID != nil {
			r.UserID = *userID
		}
		_ = json.Unmarshal(detectedRaw, &r.Detected)
		_ = json.Unmarshal(secondpassRaw, &r.SecondpassLabels)
		_ = json.Unmarshal(goldRaw, &r.GoldLabels)
		out = append(out, r)
	}
	return out, rows.Err()
}
