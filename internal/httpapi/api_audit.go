package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rromenskyi/ipsupport-airllm/internal/capture"
)

// captureReader provides read access to the capture index. Implemented by
// *capture.PGInserter for production and by fakes in tests.
type captureReader interface {
	List(ctx context.Context, f capture.ListFilter) ([]capture.IndexRow, error)
	Get(ctx context.Context, id string) (capture.IndexRow, error)
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

// auditRoutes registers the auditor-gated capture endpoints.
func (s *Server) auditRoutes() {
	a := s.requireAuditor
	s.mux.HandleFunc("GET /api/audit/captures", a(s.handleAuditListCaptures))
	s.mux.HandleFunc("GET /api/audit/captures/{id}", a(s.handleAuditGetCapture))
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
