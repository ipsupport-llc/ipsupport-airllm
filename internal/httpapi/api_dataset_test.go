package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rromenskyi/ipsupport-airllm/internal/auth"
	"github.com/rromenskyi/ipsupport-airllm/internal/capture"
	"github.com/rromenskyi/ipsupport-airllm/internal/dlp"
	"github.com/rromenskyi/ipsupport-airllm/internal/secrets"
)

// newDatasetTestServer builds a minimal Server with the admin dataset export
// route registered for use in tests.
func newDatasetTestServer(
	t *testing.T,
	principal auth.Principal,
	store captureReader,
	bs *fakeMemBlob,
	sl *secrets.Sealer,
) (*Server, *[]string) {
	t.Helper()
	var auditLog []string
	s := &Server{
		mux:        http.NewServeMux(),
		auth:       &fakeAuth{principal: principal},
		sealer:     sl,
		captureIdx: store,
		blobStore:  bs,
		auditHook: func(_ context.Context, _, action, target string, _ any) {
			auditLog = append(auditLog, action+":"+target)
		},
		ensureUserFn: func(_ context.Context, p auth.Principal) (string, error) {
			return "test-uid-" + p.Subject, nil
		},
	}
	// Register only the dataset export route to avoid needing s.st.
	s.mux.HandleFunc("POST /api/admin/dataset/export",
		s.requireAdmin(s.handleAdminDatasetExport))
	return s, &auditLog
}

// TestDatasetExportReturnsArtifact verifies that POST /api/admin/dataset/export
// returns 200 with artifact_key and count, and writes an audit event.
func TestDatasetExportReturnsArtifact(t *testing.T) {
	sl := testAuditSealer(t)

	// Build a sealed body for a "confirmed" capture.
	msgText := "key: sk-test-1234567890abcdefghijk"
	rawBody, _ := json.Marshal(map[string]any{
		"messages": []map[string]any{
			{"role": "user", "content": msgText},
		},
		"response": "ok",
	})
	sealed, err := sl.Seal(rawBody)
	if err != nil {
		t.Fatal(err)
	}

	bs := newFakeMemBlob()
	if err := bs.Put(context.Background(), "captures/ex1", sealed); err != nil {
		t.Fatal(err)
	}

	store := &fakeCaptureStore{rows: []capture.IndexRow{
		{
			ID:           "ex1",
			BlobKey:      "captures/ex1",
			ReviewStatus: "confirmed",
			Detected:     []dlp.Finding{{Label: "openai_key", Start: 5, End: len(msgText)}},
		},
	}}

	admin := auth.Principal{Subject: "admin1", Roles: []string{auth.AdminRole}}
	srv, auditLog := newDatasetTestServer(t, admin, store, bs, sl)

	req := httptest.NewRequest("POST", "/api/admin/dataset/export", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if _, ok := resp["artifact_key"]; !ok {
		t.Error("response missing artifact_key")
	}
	count, ok := resp["count"].(float64)
	if !ok || count < 1 {
		t.Errorf("expected count >= 1, got %v", resp["count"])
	}

	// Audit event must be logged.
	if len(*auditLog) == 0 || (*auditLog)[0][:len("dataset.export")] != "dataset.export" {
		t.Errorf("expected dataset.export audit event, got %v", *auditLog)
	}
}

// TestDatasetExportRequiresAdmin verifies that a non-admin gets 403.
func TestDatasetExportRequiresAdmin(t *testing.T) {
	sl := testAuditSealer(t)
	store := &fakeCaptureStore{}
	user := auth.Principal{Subject: "u1", Roles: []string{auth.UserRole}}
	srv, _ := newDatasetTestServer(t, user, store, newFakeMemBlob(), sl)

	req := httptest.NewRequest("POST", "/api/admin/dataset/export", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
}

// TestDatasetExportNoBlobStore verifies that 503 is returned when the blob
// store is not configured.
func TestDatasetExportNoBlobStore(t *testing.T) {
	sl := testAuditSealer(t)
	store := &fakeCaptureStore{}
	admin := auth.Principal{Subject: "a1", Roles: []string{auth.AdminRole}}

	var auditLog []string
	s := &Server{
		mux:        http.NewServeMux(),
		auth:       &fakeAuth{principal: admin},
		sealer:     sl,
		captureIdx: store,
		blobStore:  nil, // intentionally missing
		auditHook: func(_ context.Context, _, action, target string, _ any) {
			auditLog = append(auditLog, action+":"+target)
		},
		ensureUserFn: func(_ context.Context, p auth.Principal) (string, error) {
			return "uid-" + p.Subject, nil
		},
	}
	s.mux.HandleFunc("POST /api/admin/dataset/export",
		s.requireAdmin(s.handleAdminDatasetExport))

	req := httptest.NewRequest("POST", "/api/admin/dataset/export", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", rec.Code, rec.Body.String())
	}
}
