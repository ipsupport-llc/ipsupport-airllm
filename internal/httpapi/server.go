// Package httpapi exposes the control-plane REST API, the data-plane
// gateway endpoints, and (later) the embedded SPA, behind one mux.
package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/auth"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/blob"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/capture"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/config"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/ledger"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/limits"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/metrics"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/pricing"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/providers"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/routing"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/secrets"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/store"
)

// oidcHandler is the interface for OIDC SSO handlers (optional — nil = no SSO routes).
type oidcHandler interface {
	LoginStart(http.ResponseWriter, *http.Request)
	Callback(http.ResponseWriter, *http.Request)
}

// Deps are the runtime dependencies wired into the server.
type Deps struct {
	Providers *providers.Registry
	Limiter   *limits.Limiter
	Pricing   *pricing.Table
	Sealer    *secrets.Sealer
	Auth      auth.Authenticator
	Login     auth.LoginProvider // nil when not using password login (e.g. OIDC)
	OIDC      oidcHandler        // nil when not using OIDC
	Capture   *capture.Pipeline  // nil disables capture
	Blob      blob.Store         // for audit transcript reads; nil disables body fetch
}

// Server is the top-level HTTP handler.
type Server struct {
	cfg           *config.Config
	st            *store.Store
	mux           *http.ServeMux
	regPtr        atomic.Pointer[providers.Registry] // swapped on provider changes
	dlpPtr        atomic.Pointer[dlpConfig]          // swapped on DLP config changes
	capturePtr    atomic.Pointer[captureConfig]      // swapped on capture config changes
	secondpassPtr atomic.Pointer[secondpassConfig]   // swapped on secondpass config changes
	router        *routing.Router
	limiter       *limits.Limiter
	pricing       *pricing.Table
	sealer        *secrets.Sealer
	ledger        *ledger.Ledger
	auth          auth.Authenticator
	login         auth.LoginProvider
	oidc          oidcHandler
	httpc         *http.Client      // shared client for the DLP model sidecar
	capturePl     *capture.Pipeline // nil when capture is not configured
	blobStore     blob.Store        // nil when blob store is not configured
	captureIdx    captureReader     // nil until first audit route access (set in NewServer)
	metrics       *metrics.Metrics

	// Test hooks: non-nil values replace the real implementations in tests.
	auditHook    func(ctx context.Context, actor, action, target string, detail any)
	ensureUserFn func(ctx context.Context, p auth.Principal) (string, error)
}

// NewServer builds the routed handler.
func NewServer(cfg *config.Config, st *store.Store, deps Deps) *Server {
	s := &Server{
		cfg:     cfg,
		st:      st,
		mux:     http.NewServeMux(),
		router:  routing.NewRouter(st),
		limiter: deps.Limiter,
		pricing: deps.Pricing,
		sealer:  deps.Sealer,
		ledger:  ledger.New(st),
		auth:    deps.Auth,
		login:   deps.Login,
		oidc:    deps.OIDC,
		httpc:   &http.Client{},
		metrics: metrics.New(),
	}
	s.regPtr.Store(deps.Providers)
	s.loadDLP(context.Background())
	s.loadCapture(context.Background())
	s.loadSecondpass(context.Background())
	if deps.Capture != nil {
		s.capturePl = deps.Capture
	}
	if deps.Blob != nil {
		s.blobStore = deps.Blob
	}
	s.captureIdx = &captureIndex{pg: st.PG}
	s.routes()
	return s
}

// reg returns the current provider registry.
func (s *Server) reg() *providers.Registry { return s.regPtr.Load() }

// reloadProviders rebuilds the registry from the DB (after a provider change).
func (s *Server) reloadProviders(ctx context.Context) error {
	reg, err := providers.LoadFromStore(ctx, s.st, s.sealer)
	if err != nil {
		return err
	}
	s.regPtr.Store(reg)
	return nil
}

// maxRequestBody caps request bodies to bound memory. It is generous enough
// for large prompts but blocks pathological payloads.
const maxRequestBody = 16 << 20 // 16 MiB

// statusRecorder captures the response status for metrics.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// ingressOf maps a request path to a metrics ingress label.
func ingressOf(path string) string {
	switch path {
	case "/v1/chat/completions", "/v1/models":
		return "openai"
	case "/v1/messages":
		return "anthropic"
	default:
		return "control"
	}
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	}
	start := time.Now()
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	s.mux.ServeHTTP(rec, r)
	s.metrics.RecordRequest(ingressOf(r.URL.Path), rec.status, time.Since(start))
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", s.handleHealth)
	s.mux.HandleFunc("GET /readyz", s.handleReady)
	s.mux.Handle("GET /metrics", s.metrics.Handler())

	// Data-plane (API-key auth).
	s.mux.HandleFunc("POST /v1/chat/completions", s.requireAPIKey(s.handleChatCompletions))
	s.mux.HandleFunc("GET /v1/models", s.requireAPIKey(s.handleModels))
	s.mux.HandleFunc("POST /v1/messages", s.requireAPIKey(s.handleMessages))

	// Control-plane auth endpoints (public — no session required).
	s.mux.HandleFunc("GET /api/auth/mode", s.handleAuthMode)
	if s.login != nil {
		s.mux.HandleFunc("POST /auth/login", s.handleLogin)
		s.mux.HandleFunc("POST /auth/logout", s.handleLogout)
	}
	if s.oidc != nil {
		s.mux.HandleFunc("GET /auth/sso", s.oidc.LoginStart)
		s.mux.HandleFunc("GET /auth/callback", s.oidc.Callback)
	}

	// Control-plane self-service (session auth).
	s.mux.HandleFunc("GET /api/me", s.requireSession(s.handleMe))
	s.mux.HandleFunc("GET /api/keys", s.requireSession(s.handleListKeys))
	s.mux.HandleFunc("POST /api/keys", s.requireSession(s.handleCreateKey))
	s.mux.HandleFunc("POST /api/keys/{id}/revoke", s.requireSession(s.handleRevokeKey))
	s.mux.HandleFunc("GET /api/usage", s.requireSession(s.handleUsage))
	s.mux.HandleFunc("POST /api/me/password", s.requireSession(s.handleChangeOwnPassword))

	s.adminRoutes()
	s.auditRoutes()

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
