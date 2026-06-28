// Package httpapi exposes the control-plane REST API, the data-plane
// gateway endpoints, and (later) the embedded SPA, behind one mux.
package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/rromenskyi/ipsupport-airouter/internal/config"
	"github.com/rromenskyi/ipsupport-airouter/internal/ledger"
	"github.com/rromenskyi/ipsupport-airouter/internal/providers"
	"github.com/rromenskyi/ipsupport-airouter/internal/store"
)

// Server is the top-level HTTP handler.
type Server struct {
	cfg       *config.Config
	st        *store.Store
	mux       *http.ServeMux
	providers *providers.Registry
	ledger    *ledger.Ledger
}

// NewServer builds the routed handler with a provider registry and ledger.
func NewServer(cfg *config.Config, st *store.Store) *Server {
	reg := providers.NewRegistry()
	reg.Register(providers.NewMock("mock"))

	s := &Server{
		cfg:       cfg,
		st:        st,
		mux:       http.NewServeMux(),
		providers: reg,
		ledger:    ledger.New(st),
	}
	s.routes()
	return s
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", s.handleHealth)
	s.mux.HandleFunc("GET /readyz", s.handleReady)

	// Data-plane (API-key auth).
	s.mux.HandleFunc("POST /v1/chat/completions", s.requireAPIKey(s.handleChatCompletions))
	s.mux.HandleFunc("GET /v1/models", s.requireAPIKey(s.handleModels))
}

// resolve maps a client-requested model to a provider and upstream model.
//
// Phase 1 stub: always the mock provider, with the requested model passed
// through as the upstream model. Alias-catalog routing with fallback lands
// in phase 3.
func (s *Server) resolve(model string) (providers.Provider, string) {
	p, _ := s.providers.Get("mock")
	return p, model
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
