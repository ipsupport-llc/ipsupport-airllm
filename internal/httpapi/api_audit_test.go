package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rromenskyi/ipsupport-airllm/internal/auth"
	"github.com/rromenskyi/ipsupport-airllm/internal/blob"
	"github.com/rromenskyi/ipsupport-airllm/internal/capture"
	"github.com/rromenskyi/ipsupport-airllm/internal/secrets"
)

// --- fakes ---

// fakeAuth returns a fixed principal for any request.
type fakeAuth struct {
	principal auth.Principal
}

func (f *fakeAuth) Authenticate(_ *http.Request) (auth.Principal, error) {
	return f.principal, nil
}

// fakeCaptureStore is a test double for captureReader.
type fakeCaptureStore struct {
	mu   sync.Mutex
	rows []capture.IndexRow
}

func (f *fakeCaptureStore) List(_ context.Context, _ capture.ListFilter) ([]capture.IndexRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rows, nil
}

func (f *fakeCaptureStore) Get(_ context.Context, id string) (capture.IndexRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.rows {
		if r.ID == id {
			return r, nil
		}
	}
	return capture.IndexRow{}, capture.ErrNotFound
}

// fakeMemBlob is a simple in-memory blob.Store.
type fakeMemBlob struct {
	mu   sync.Mutex
	data map[string][]byte
}

func newFakeMemBlob() *fakeMemBlob { return &fakeMemBlob{data: map[string][]byte{}} }

func (b *fakeMemBlob) Put(_ context.Context, key string, val []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	cp := make([]byte, len(val))
	copy(cp, val)
	b.data[key] = cp
	return nil
}

func (b *fakeMemBlob) Get(_ context.Context, key string) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	d, ok := b.data[key]
	if !ok {
		return nil, blob.ErrNotFound
	}
	return d, nil
}

func (b *fakeMemBlob) Delete(_ context.Context, key string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.data[key]; !ok {
		return blob.ErrNotFound
	}
	delete(b.data, key)
	return nil
}

// --- test helpers ---

func testAuditSealer(t *testing.T) *secrets.Sealer {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 3)
	}
	s, err := secrets.New(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// newAuditTestServer builds a minimal Server wired for audit handler tests.
// auditPrincipal is what the fake auth returns.
func newAuditTestServer(
	t *testing.T,
	principal auth.Principal,
	store captureReader,
	bs blob.Store,
) (*Server, *[]string) {
	t.Helper()
	sealer := testAuditSealer(t)
	var auditLog []string
	s := &Server{
		mux:        http.NewServeMux(),
		auth:       &fakeAuth{principal: principal},
		sealer:     sealer,
		captureIdx: store,
		blobStore:  bs,
		auditHook: func(_ context.Context, _, action, target string, _ any) {
			auditLog = append(auditLog, action+":"+target)
		},
		ensureUserFn: func(_ context.Context, p auth.Principal) (string, error) {
			return "test-uid-" + p.Subject, nil
		},
	}
	s.auditRoutes()
	return s, &auditLog
}

// --- tests ---

func TestAuditListCaptures(t *testing.T) {
	store := &fakeCaptureStore{rows: []capture.IndexRow{
		{ID: "aaa", TS: time.Now(), IngressProtocol: "openai", ReviewStatus: "unreviewed"},
		{ID: "bbb", TS: time.Now(), IngressProtocol: "anthropic", ReviewStatus: "reviewed"},
	}}
	auditor := auth.Principal{Subject: "tester", Roles: []string{auth.AuditorRole}}
	srv, _ := newAuditTestServer(t, auditor, store, newFakeMemBlob())

	req := httptest.NewRequest("GET", "/api/audit/captures", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var body map[string]json.RawMessage
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	var captures []capture.IndexRow
	if err := json.Unmarshal(body["captures"], &captures); err != nil {
		t.Fatal(err)
	}
	if len(captures) != 2 {
		t.Fatalf("expected 2 captures, got %d", len(captures))
	}
}

func TestAuditGetCaptureDecryptsBodyAndLogs(t *testing.T) {
	sealer := testAuditSealer(t)
	plaintext := []byte(`{"messages":[{"role":"user","content":"hello"}],"response":"hi"}`)
	sealed, err := sealer.Seal(plaintext)
	if err != nil {
		t.Fatal(err)
	}

	bs := newFakeMemBlob()
	_ = bs.Put(context.Background(), "captures/row1", sealed)

	store := &fakeCaptureStore{rows: []capture.IndexRow{
		{ID: "row1", BlobKey: "captures/row1", ReviewStatus: "unreviewed"},
	}}
	auditor := auth.Principal{Subject: "auditor1", Roles: []string{auth.AuditorRole}}
	srv, auditLog := newAuditTestServer(t, auditor, store, bs)

	req := httptest.NewRequest("GET", "/api/audit/captures/row1", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]json.RawMessage
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	var bodyStr string
	if err := json.Unmarshal(resp["body"], &bodyStr); err != nil {
		t.Fatalf("body field not a string: %v", err)
	}
	if !strings.Contains(bodyStr, "hello") {
		t.Errorf("decrypted body should contain original content, got: %q", bodyStr)
	}
	// Audit row must have been written.
	if len(*auditLog) == 0 {
		t.Error("expected audit.view to be logged")
	}
	if (*auditLog)[0] != "audit.view:row1" {
		t.Errorf("unexpected audit log entry: %v", *auditLog)
	}
}

func TestAuditGetCaptureNotFound(t *testing.T) {
	store := &fakeCaptureStore{}
	auditor := auth.Principal{Subject: "a", Roles: []string{auth.AuditorRole}}
	srv, _ := newAuditTestServer(t, auditor, store, newFakeMemBlob())

	req := httptest.NewRequest("GET", "/api/audit/captures/nope", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestAuditRequiresAuditorRole(t *testing.T) {
	store := &fakeCaptureStore{}
	user := auth.Principal{Subject: "u", Roles: []string{auth.UserRole}}
	srv, _ := newAuditTestServer(t, user, store, newFakeMemBlob())

	for _, path := range []string{"/api/audit/captures", "/api/audit/captures/any"} {
		req := httptest.NewRequest("GET", path, nil)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: expected 403, got %d", path, rec.Code)
		}
	}
}

func TestAuditAdminPassesAuditorCheck(t *testing.T) {
	store := &fakeCaptureStore{}
	admin := auth.Principal{Subject: "admin", Roles: []string{auth.AdminRole}}
	srv, _ := newAuditTestServer(t, admin, store, newFakeMemBlob())

	req := httptest.NewRequest("GET", "/api/audit/captures", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("admin should pass auditor check, got %d", rec.Code)
	}
}
