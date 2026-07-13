package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/auth"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/pricing"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/providers"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/store"
)

// TestAdminPricingImport imports the Mock provider's fixed priced catalog
// entry through the real endpoint against a real dev-stack database, and
// asserts the row lands in the pricing table. Gated on TEST_DATABASE_URL;
// the dev Postgres is shared, so the fixture row is deleted in t.Cleanup.
func TestAdminPricingImport(t *testing.T) {
	pool := testPool(t)

	reg := providers.NewRegistry()
	reg.Register(providers.NewMock("mock"), 0)

	s := &Server{
		mux:  http.NewServeMux(),
		st:   &store.Store{PG: pool},
		auth: &fakeAuth{principal: auth.Principal{Subject: "admin", Roles: []string{auth.AdminRole}}},
		ensureUserFn: func(_ context.Context, p auth.Principal) (string, error) {
			return "uid-" + p.Subject, nil
		},
		pricing: pricing.New(),
	}
	s.regPtr.Store(reg)
	s.mux.HandleFunc("POST /api/admin/pricing/import/{provider}", s.requireAdmin(s.handleAdminPricingImport))

	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM pricing WHERE provider = 'mock' AND model = 'mock-gpt'`)
	})

	req := httptest.NewRequest(http.MethodPost, "/api/admin/pricing/import/mock", nil)
	rw := httptest.NewRecorder()
	s.mux.ServeHTTP(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s, want 200", rw.Code, rw.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rw.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if imported, _ := body["imported"].(float64); imported != 1 {
		t.Errorf("imported = %v, want 1", body["imported"])
	}

	var inPer1M, outPer1M float64
	err := pool.QueryRow(context.Background(),
		`SELECT input_per_1m, output_per_1m FROM pricing WHERE provider = 'mock' AND model = 'mock-gpt'`).
		Scan(&inPer1M, &outPer1M)
	if err != nil {
		t.Fatalf("row not found after import: %v", err)
	}
	if inPer1M != 1 || outPer1M != 2 {
		t.Errorf("pricing row = (%v, %v), want (1, 2)", inPer1M, outPer1M)
	}

	if s.pricing.CostMicroUSD("mock", "mock-gpt", 1_000_000, 1_000_000) != 3_000_000 {
		t.Errorf("in-memory table not updated after import (Set not called or wrong values)")
	}
}

func TestAdminPricingImportUnknownProvider(t *testing.T) {
	pool := testPool(t)

	s := &Server{
		mux:  http.NewServeMux(),
		st:   &store.Store{PG: pool},
		auth: &fakeAuth{principal: auth.Principal{Subject: "admin", Roles: []string{auth.AdminRole}}},
		ensureUserFn: func(_ context.Context, p auth.Principal) (string, error) {
			return "uid-" + p.Subject, nil
		},
		pricing: pricing.New(),
	}
	s.regPtr.Store(providers.NewRegistry())
	s.mux.HandleFunc("POST /api/admin/pricing/import/{provider}", s.requireAdmin(s.handleAdminPricingImport))

	req := httptest.NewRequest(http.MethodPost, "/api/admin/pricing/import/ghost", nil)
	rw := httptest.NewRecorder()
	s.mux.ServeHTTP(rw, req)
	if rw.Code != http.StatusNotFound {
		t.Errorf("code = %d, want 404", rw.Code)
	}
}

func TestAdminPricingImportUnsupportedProvider(t *testing.T) {
	pool := testPool(t)

	reg := providers.NewRegistry()
	reg.Register(&bareProvider{Provider: providers.NewMock("bare")}, 0)

	s := &Server{
		mux:  http.NewServeMux(),
		st:   &store.Store{PG: pool},
		auth: &fakeAuth{principal: auth.Principal{Subject: "admin", Roles: []string{auth.AdminRole}}},
		ensureUserFn: func(_ context.Context, p auth.Principal) (string, error) {
			return "uid-" + p.Subject, nil
		},
		pricing: pricing.New(),
	}
	s.regPtr.Store(reg)
	s.mux.HandleFunc("POST /api/admin/pricing/import/{provider}", s.requireAdmin(s.handleAdminPricingImport))

	req := httptest.NewRequest(http.MethodPost, "/api/admin/pricing/import/bare", nil)
	rw := httptest.NewRecorder()
	s.mux.ServeHTTP(rw, req)
	if rw.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rw.Code)
	}
	var body map[string]any
	json.Unmarshal(rw.Body.Bytes(), &body)
	if body["unsupported"] != true {
		t.Errorf("unsupported = %v, want true", body["unsupported"])
	}
	if imported, _ := body["imported"].(float64); imported != 0 {
		t.Errorf("imported = %v, want 0", body["imported"])
	}
}
