// Package httpapi exposes the control-plane REST API, the data-plane
// gateway endpoints, and (later) the embedded SPA, behind one mux.
package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/rromenskyi/ipsupport-airllm/internal/auth"
	"github.com/rromenskyi/ipsupport-airllm/internal/config"
	"github.com/rromenskyi/ipsupport-airllm/internal/ledger"
	"github.com/rromenskyi/ipsupport-airllm/internal/limits"
	"github.com/rromenskyi/ipsupport-airllm/internal/pricing"
	"github.com/rromenskyi/ipsupport-airllm/internal/providers"
	"github.com/rromenskyi/ipsupport-airllm/internal/routing"
	"github.com/rromenskyi/ipsupport-airllm/internal/secrets"
	"github.com/rromenskyi/ipsupport-airllm/internal/store"
)

// Deps are the runtime dependencies wired into the server.
type Deps struct {
	Providers *providers.Registry
	Limiter   *limits.Limiter
	Pricing   *pricing.Table
	Sealer    *secrets.Sealer
	Auth      auth.Authenticator
	Login     auth.LoginProvider // nil when not using password login (e.g. OIDC)
}

// Server is the top-level HTTP handler.
type Server struct {
	cfg       *config.Config
	st        *store.Store
	mux       *http.ServeMux
	providers *providers.Registry
	router    *routing.Router
	limiter   *limits.Limiter
	pricing   *pricing.Table
	sealer    *secrets.Sealer
	ledger    *ledger.Ledger
	auth      auth.Authenticator
	login     auth.LoginProvider
}

// NewServer builds the routed handler.
func NewServer(cfg *config.Config, st *store.Store, deps Deps) *Server {
	s := &Server{
		cfg:       cfg,
		st:        st,
		mux:       http.NewServeMux(),
		providers: deps.Providers,
		router:    routing.NewRouter(st),
		limiter:   deps.Limiter,
		pricing:   deps.Pricing,
		sealer:    deps.Sealer,
		ledger:    ledger.New(st),
		auth:      deps.Auth,
		login:     deps.Login,
	}
	s.routes()
	return s
}

// maxRequestBody caps request bodies to bound memory. It is generous enough
// for large prompts but blocks pathological payloads.
const maxRequestBody = 16 << 20 // 16 MiB

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	}
	s.mux.ServeHTTP(w, r)
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", s.handleHealth)
	s.mux.HandleFunc("GET /readyz", s.handleReady)

	// Data-plane (API-key auth).
	s.mux.HandleFunc("POST /v1/chat/completions", s.requireAPIKey(s.handleChatCompletions))
	s.mux.HandleFunc("GET /v1/models", s.requireAPIKey(s.handleModels))
	s.mux.HandleFunc("POST /v1/messages", s.requireAPIKey(s.handleMessages))

	// Control-plane auth (password login present only in mock mode).
	if s.login != nil {
		s.mux.HandleFunc("POST /auth/login", s.handleLogin)
		s.mux.HandleFunc("POST /auth/logout", s.handleLogout)
	}

	// Control-plane self-service (session auth).
	s.mux.HandleFunc("GET /api/me", s.requireSession(s.handleMe))
	s.mux.HandleFunc("GET /api/keys", s.requireSession(s.handleListKeys))
	s.mux.HandleFunc("POST /api/keys", s.requireSession(s.handleCreateKey))
	s.mux.HandleFunc("POST /api/keys/{id}/revoke", s.requireSession(s.handleRevokeKey))
	s.mux.HandleFunc("GET /api/usage", s.requireSession(s.handleUsage))

	s.adminRoutes()

	// Static SPA (catch-all GET; API prefixes excluded inside).
	s.registerSPA()
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	if err := s.st.PG.Ping(r.Context()); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "postgres unavailable"})
		return
	}
	if err := s.st.RDB.Ping(r.Context()).Err(); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "redis unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
