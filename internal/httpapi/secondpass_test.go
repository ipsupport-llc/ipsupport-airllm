package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/auth"
)

func TestDefaultSecondpassConfigDisabled(t *testing.T) {
	cfg := defaultSecondpassConfig()
	if cfg.Enabled {
		t.Error("secondpass must be disabled by default")
	}
	if cfg.Model != "" {
		t.Errorf("default Model must be empty, got %q", cfg.Model)
	}
	if cfg.IntervalSec <= 0 {
		t.Errorf("default IntervalSec must be positive, got %d", cfg.IntervalSec)
	}
	if cfg.MinScore <= 0 || cfg.MinScore > 1 {
		t.Errorf("default MinScore must be in (0,1], got %f", cfg.MinScore)
	}
}

func TestSecondpassCfgAtomicLoad(t *testing.T) {
	s := &Server{}
	// Before any store: should return default.
	cfg := s.secondpassCfg()
	if cfg.Enabled {
		t.Error("uninitialized server must return disabled secondpass config")
	}

	// After storing a custom config, secondpassCfg must return it.
	want := secondpassConfig{Enabled: true, Model: "gpt-4", IntervalSec: 30, MinScore: 0.8}
	s.secondpassPtr.Store(&want)
	got := s.secondpassCfg()
	if got.Enabled != want.Enabled || got.Model != want.Model {
		t.Errorf("secondpassCfg returned wrong config: %+v", got)
	}
}

func newSecondpassTestServer(t *testing.T, principal auth.Principal) *Server {
	t.Helper()
	s := &Server{
		mux:       http.NewServeMux(),
		auth:      &fakeAuth{principal: principal},
		auditHook: func(_ context.Context, _, _, _ string, _ any) {},
		ensureUserFn: func(_ context.Context, p auth.Principal) (string, error) {
			return "uid-" + p.Subject, nil
		},
	}
	s.adminRoutes()
	return s
}

func TestHandleAdminGetSecondpass_ReturnsDefault(t *testing.T) {
	admin := auth.Principal{Subject: "admin", Roles: []string{auth.AdminRole}}
	s := newSecondpassTestServer(t, admin)

	req := httptest.NewRequest("GET", "/api/admin/secondpass", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var cfg secondpassConfig
	if err := json.NewDecoder(rec.Body).Decode(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Enabled {
		t.Error("default secondpass config must be disabled")
	}
}

func TestHandleAdminGetSecondpass_NonAdmin403(t *testing.T) {
	user := auth.Principal{Subject: "user1", Roles: []string{}} // no admin role
	s := newSecondpassTestServer(t, user)

	req := httptest.NewRequest("GET", "/api/admin/secondpass", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for non-admin, got %d", rec.Code)
	}
}

func TestHandleAdminPutSecondpass_BadBody(t *testing.T) {
	admin := auth.Principal{Subject: "admin", Roles: []string{auth.AdminRole}}
	s := newSecondpassTestServer(t, admin)

	req := httptest.NewRequest("PUT", "/api/admin/secondpass",
		strings.NewReader("{invalid json"))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for bad body, got %d", rec.Code)
	}
}
