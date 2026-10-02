package httpapi

import (
	"context"
	"net/http"
	"path"
	"strings"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/blob"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/capture"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/dataset"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/secrets"
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

// sealedBlobWriter seals data with the provided sealer before writing it to
// blob storage — the symmetric counterpart to readDecryptedBlob, so a
// dataset export artifact (built from decrypted capture bodies) is never
// written to storage in plaintext the way every other blob in this codebase
// is sealed at rest.
type sealedBlobWriter struct {
	bs blob.Store
	sl *secrets.Sealer
}

func (w sealedBlobWriter) Put(ctx context.Context, key string, data []byte) error {
	sealed, err := w.sl.Seal(data)
	if err != nil {
		return err
	}
	return w.bs.Put(ctx, key, sealed)
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
		sealedBlobWriter{bs: s.blobStore, sl: s.sealer},
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

// handleAdminDatasetDownload decrypts and streams back a dataset export
// artifact. Export artifacts are sealed at rest like every other blob in
// this codebase (see sealedBlobWriter), so this is the only way to retrieve
// one — a plain filesystem/object-store `cat` of the blob now yields
// ciphertext, not JSONL.
// GET /api/admin/dataset/download?key=<artifact_key>
func (s *Server) handleAdminDatasetDownload(w http.ResponseWriter, r *http.Request) {
	sess, _ := sessionFrom(r.Context())

	if s.blobStore == nil || s.sealer == nil {
		writeControlError(w, http.StatusServiceUnavailable, "blob store not configured")
		return
	}

	key := r.URL.Query().Get("key")
	// Scoped to the dataset-export prefix only: this handler must never
	// become a general decrypt-any-blob-by-key oracle over capture bodies.
	if key == "" || !strings.HasPrefix(key, "datasets/") {
		writeControlError(w, http.StatusBadRequest, "invalid key")
		return
	}

	body, err := readDecryptedBlob(r.Context(), s.blobStore, s.sealer, key)
	if err != nil {
		writeControlError(w, http.StatusNotFound, "artifact not found")
		return
	}

	s.audit(r.Context(), sess.principal.Subject, "dataset.download", key, nil)

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Content-Disposition", `attachment; filename="`+path.Base(key)+`"`)
	w.Write(body)
}
