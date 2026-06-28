package capture

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rromenskyi/ipsupport-airllm/internal/dlp"
)

// IndexRow mirrors the capture_index table.
type IndexRow struct {
	ID               string
	TS               time.Time
	KeyID            string
	UserID           string
	IngressProtocol  string
	Alias            string
	ProviderName     string
	UpstreamModel    string
	Status           int
	PromptTokens     int64
	CompletionTokens int64
	CostUSD          float64
	BlobKey          string
	Redacted         bool
	ModelVersion     string
	Detected         []dlp.Finding
	ReviewStatus     string
	SecondpassStatus string
	SecondpassLabels []dlp.Finding
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
		labels = []byte("{}")
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
		       detected, review_status, secondpass_status, secondpass_labels
		FROM capture_index
		WHERE ($1 = '' OR review_status = $1)
		  AND ($2 = '' OR secondpass_status = $2)
		ORDER BY ts DESC
		LIMIT $3`,
		f.ReviewStatus, f.SecondpassStatus, limit)
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
		       detected, review_status, secondpass_status, secondpass_labels
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
		return IndexRow{}, pgxNotFound()
	}
	return out[0], nil
}

// pgxNotFound returns a sentinel error for missing rows.
func pgxNotFound() error {
	return errNotFound
}

type notFoundError struct{}

func (notFoundError) Error() string { return "capture: not found" }

// ErrNotFound is returned by Get when no row matches.
var errNotFound = notFoundError{}

func scanRows(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
}) ([]IndexRow, error) {
	var out []IndexRow
	for rows.Next() {
		var r IndexRow
		var keyID, userID *string
		var detectedRaw, secondpassRaw []byte
		if err := rows.Scan(
			&r.ID, &r.TS, &keyID, &userID,
			&r.IngressProtocol, &r.Alias,
			&r.ProviderName, &r.UpstreamModel, &r.Status,
			&r.PromptTokens, &r.CompletionTokens, &r.CostUSD,
			&r.BlobKey, &r.Redacted, &r.ModelVersion,
			&detectedRaw, &r.ReviewStatus, &r.SecondpassStatus, &secondpassRaw,
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
		out = append(out, r)
	}
	return out, rows.Err()
}
