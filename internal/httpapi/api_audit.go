package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rromenskyi/ipsupport-airllm/internal/capture"
	"github.com/rromenskyi/ipsupport-airllm/internal/dlp"
)

// captureReader provides read and review access to the capture index.
// Implemented by *capture.PGInserter for production and by fakes in tests.
type captureReader interface {
	List(ctx context.Context, f capture.ListFilter) ([]capture.IndexRow, error)
	Get(ctx context.Context, id string) (capture.IndexRow, error)
	ReviewQueue(ctx context.Context, limit int) ([]capture.IndexRow, error)
	SetReview(ctx context.Context, id string, reviewStatus string, goldLabels []dlp.Finding) error
}

// captureIndex wraps pgxpool.Pool to implement captureReader via PGInserter.
type captureIndex struct {
	pg *pgxpool.Pool
}

func (c *captureIndex) List(ctx context.Context, f capture.ListFilter) ([]capture.IndexRow, error) {
	return (&capture.PGInserter{PG: c.pg}).List(ctx, f)
}

func (c *captureIndex) Get(ctx context.Context, id string) (capture.IndexRow, error) {
	return (&capture.PGInserter{PG: c.pg}).Get(ctx, id)
}

func (c *captureIndex) ReviewQueue(ctx context.Context, limit int) ([]capture.IndexRow, error) {
	return (&capture.PGInserter{PG: c.pg}).ReviewQueue(ctx, limit)
}

func (c *captureIndex) SetReview(ctx context.Context, id string, reviewStatus string, goldLabels []dlp.Finding) error {
	return (&capture.PGInserter{PG: c.pg}).SetReview(ctx, id, reviewStatus, goldLabels)
}

// auditRoutes registers the auditor-gated capture endpoints.
func (s *Server) auditRoutes() {
	a := s.requireAuditor
	s.mux.HandleFunc("GET /api/audit/captures", a(s.handleAuditListCaptures))
	s.mux.HandleFunc("GET /api/audit/captures/{id}", a(s.handleAuditGetCapture))
	s.mux.HandleFunc("GET /api/audit/review", a(s.handleAuditReviewQueue))
	s.mux.HandleFunc("POST /api/audit/captures/{id}/review", a(s.handleAuditPostReview))
}

// handleAuditListCaptures lists capture_index rows (metadata + DLP labels, NO body).
// Query params: from (RFC3339), to (RFC3339), review_status, limit.
func (s *Server) handleAuditListCaptures(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := capture.ListFilter{
		ReviewStatus: q.Get("review_status"),
	}
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			f.Limit = n
		}
	}
	if v := q.Get("from"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			f.From = &t
		}
	}
	if v := q.Get("to"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			f.To = &t
		}
	}

	rows, err := s.captureIdx.List(r.Context(), f)
	if err != nil {
		writeControlError(w, http.StatusInternalServerError, "failed to list captures")
		return
	}
	if rows == nil {
		rows = []capture.IndexRow{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"captures": rows})
}

// handleAuditGetCapture returns a single capture row with its decrypted body.
// Every call is audit-logged via audit.view.
func (s *Server) handleAuditGetCapture(w http.ResponseWriter, r *http.Request) {
	sess, _ := sessionFrom(r.Context())
	id := r.PathValue("id")

	row, err := s.captureIdx.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, capture.ErrNotFound) {
			writeControlError(w, http.StatusNotFound, "capture not found")
			return
		}
		writeControlError(w, http.StatusInternalServerError, "failed to get capture")
		return
	}

	// Fetch and decrypt the body from the blob store.
	var body string
	if s.blobStore != nil && row.BlobKey != "" {
		sealed, berr := s.blobStore.Get(r.Context(), row.BlobKey)
		if berr == nil {
			if plain, oerr := s.sealer.Open(sealed); oerr == nil {
				body = string(plain)
			}
		}
	}

	// Every transcript read must be audit-logged (access control requirement).
	s.audit(r.Context(), sess.principal.Subject, "audit.view", id, nil)

	writeJSON(w, http.StatusOK, map[string]any{
		"capture": row,
		"body":    body,
	})
}

// handleAuditReviewQueue returns captures pending review:
// rows where review_status='unreviewed' OR secondpass_status='suspect'.
func (s *Server) handleAuditReviewQueue(w http.ResponseWriter, r *http.Request) {
	rows, err := s.captureIdx.ReviewQueue(r.Context(), 200)
	if err != nil {
		writeControlError(w, http.StatusInternalServerError, "failed to load review queue")
		return
	}
	if rows == nil {
		rows = []capture.IndexRow{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"captures": rows})
}

// reviewRequest is the body for POST /api/audit/captures/{id}/review.
type reviewRequest struct {
	ReviewStatus string        `json:"review_status"`
	Labels       []dlp.Finding `json:"labels"`
}

// handleAuditPostReview updates review_status and gold_labels for a capture.
func (s *Server) handleAuditPostReview(w http.ResponseWriter, r *http.Request) {
	sess, _ := sessionFrom(r.Context())
	id := r.PathValue("id")

	var req reviewRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeControlError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if err := s.captureIdx.SetReview(r.Context(), id, req.ReviewStatus, req.Labels); err != nil {
		if errors.Is(err, capture.ErrInvalidReviewStatus) {
			writeControlError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeControlError(w, http.StatusInternalServerError, "failed to update review")
		return
	}

	s.audit(r.Context(), sess.principal.Subject, "audit.review", id, map[string]any{
		"review_status": req.ReviewStatus,
		"labels_count":  len(req.Labels),
	})

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
