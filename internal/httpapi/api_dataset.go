package httpapi

import (
	"context"
	"net/http"

	"github.com/rromenskyi/ipsupport-airllm/internal/blob"
	"github.com/rromenskyi/ipsupport-airllm/internal/capture"
	"github.com/rromenskyi/ipsupport-airllm/internal/dataset"
	"github.com/rromenskyi/ipsupport-airllm/internal/secrets"
)

// captureReviewedAdapter bridges captureReader → dataset.Store by running two
// queries (confirmed + false_negative) and merging the results.
type captureReviewedAdapter struct {
	idx captureReader
}

func (a *captureReviewedAdapter) ListReviewed(ctx context.Context) ([]capture.IndexRow, error) {
	confirmed, err := a.idx.List(ctx, capture.ListFilter{ReviewStatus: "confirmed", Limit: 10_000})
	if err != nil {
		return nil, err
	}
	fn, err := a.idx.List(ctx, capture.ListFilter{ReviewStatus: "false_negative", Limit: 10_000})
	if err != nil {
		return nil, err
	}
	return append(confirmed, fn...), nil
}

// readDecryptedBlob fetches a blob and decrypts it with the provided sealer.
func readDecryptedBlob(ctx context.Context, bs blob.Store, sl *secrets.Sealer, blobKey string) ([]byte, error) {
	sealed, err := bs.Get(ctx, blobKey)
	if err != nil {
		return nil, err
	}
	return sl.Open(sealed)
}

// handleAdminDatasetExport exports reviewed captures as a labeled JSONL artifact.
// POST /api/admin/dataset/export
func (s *Server) handleAdminDatasetExport(w http.ResponseWriter, r *http.Request) {
	sess, _ := sessionFrom(r.Context())

	if s.blobStore == nil || s.sealer == nil {
		writeControlError(w, http.StatusServiceUnavailable, "blob store not configured")
		return
	}

	key, count, err := dataset.Export(
		r.Context(),
		&captureReviewedAdapter{idx: s.captureIdx},
		func(ctx context.Context, blobKey string) ([]byte, error) {
			return readDecryptedBlob(ctx, s.blobStore, s.sealer, blobKey)
		},
		s.blobStore,
	)
	if err != nil {
		writeControlError(w, http.StatusInternalServerError, "export failed: "+err.Error())
		return
	}

	s.audit(r.Context(), sess.principal.Subject, "dataset.export", key, map[string]any{
		"count": count,
	})

	writeJSON(w, http.StatusOK, map[string]any{
		"artifact_key": key,
		"count":        count,
	})
}
