package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/auth"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/capture"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/dlp"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/secrets"
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
		ensureUserFn: func(_ context.Context, p auth.Principal) (ensuredUser, error) {
			return ensuredUser{id: "test-uid-" + p.Subject, roles: p.Roles}, nil
		},
	}
	// Register only the dataset export/download routes to avoid needing s.st.
	s.mux.HandleFunc("POST /api/admin/dataset/export",
		s.requireAdmin(s.handleAdminDatasetExport))
	s.mux.HandleFunc("GET /api/admin/dataset/download",
		s.requireAdmin(s.handleAdminDatasetDownload))
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

// TestDatasetExportArtifactIsSealedAndDownloadable proves the export
// artifact is encrypted at rest (DLP C1 fix) and only retrievable through
// the decrypting download endpoint, scoped to the datasets/ prefix.
func TestDatasetExportArtifactIsSealedAndDownloadable(t *testing.T) {
	sl := testAuditSealer(t)

	msgText := "key: sk-test-1234567890abcdefghijk"
	rawBody, _ := json.Marshal(map[string]any{
		"messages": []map[string]any{{"role": "user", "content": msgText}},
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
	srv, _ := newDatasetTestServer(t, admin, store, bs, sl)

	req := httptest.NewRequest("POST", "/api/admin/dataset/export", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("export: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode export response: %v", err)
	}
	artifactKey, _ := resp["artifact_key"].(string)
	if artifactKey == "" {
		t.Fatal("export response missing artifact_key")
	}

	// The blob actually written to storage must not be plaintext JSONL: it
	// must fail to decrypt with the WRONG key, and must not contain the
	// captured secret in the clear.
	raw, err := bs.Get(context.Background(), artifactKey)
	if err != nil {
		t.Fatalf("read raw artifact blob: %v", err)
	}
	if bytes.Contains(raw, []byte(msgText)) {
		t.Fatal("artifact blob contains plaintext training data — not sealed at rest")
	}
	wrongKey := make([]byte, 32)
	for i := range wrongKey {
		wrongKey[i] = byte(255 - i)
	}
	wrongSealer, err := secrets.New(wrongKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wrongSealer.Open(raw); err == nil {
		t.Fatal("artifact blob opened with the wrong key — it isn't actually sealed")
	}

	// The download endpoint must decrypt it back to the original JSONL.
	dlReq := httptest.NewRequest("GET", "/api/admin/dataset/download?key="+artifactKey, nil)
	dlRec := httptest.NewRecorder()
	srv.ServeHTTP(dlRec, dlReq)
	if dlRec.Code != http.StatusOK {
		t.Fatalf("download: expected 200, got %d: %s", dlRec.Code, dlRec.Body.String())
	}
	if !bytes.Contains(dlRec.Body.Bytes(), []byte(msgText)) {
		t.Fatalf("downloaded artifact missing expected plaintext content: %s", dlRec.Body.String())
	}

	// Must refuse to decrypt anything outside the datasets/ prefix.
	escReq := httptest.NewRequest("GET", "/api/admin/dataset/download?key=captures/ex1", nil)
	escRec := httptest.NewRecorder()
	srv.ServeHTTP(escRec, escReq)
	if escRec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a non-dataset key, got %d: %s", escRec.Code, escRec.Body.String())
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
		ensureUserFn: func(_ context.Context, p auth.Principal) (ensuredUser, error) {
			return ensuredUser{id: "uid-" + p.Subject, roles: p.Roles}, nil
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
