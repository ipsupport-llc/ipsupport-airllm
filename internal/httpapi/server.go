// Package httpapi exposes the control-plane REST API, the data-plane
// gateway endpoints, and (later) the embedded SPA, behind one mux.
package httpapi

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/affinity"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/auth"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/blob"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/breaker"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/capture"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/config"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/ledger"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/limits"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/lookupcache"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/metrics"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/modelpool"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/pricing"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/providers"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/routing"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/secrets"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/store"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/unavail"
)

// oidcHandler is the interface for OIDC SSO handlers (optional — nil = no SSO routes).
type oidcHandler interface {
	LoginStart(http.ResponseWriter, *http.Request)
	Callback(http.ResponseWriter, *http.Request)
}

// Deps are the runtime dependencies wired into the server.
type Deps struct {
	Providers *providers.Registry // nil = empty until ReloadProviders
	Limiter   *limits.Limiter
	Pricing   *pricing.Table
	Sealer    *secrets.Sealer
	Auth      auth.Authenticator
	Login     auth.LoginProvider // nil when not using password login (e.g. OIDC)
	OIDC      oidcHandler        // nil when not using OIDC
	Capture   *capture.Pipeline  // nil disables capture
	Blob      blob.Store         // for audit transcript reads; nil disables body fetch
	Now       func() time.Time   // clock for the lookup caches; nil = time.Now (tests inject)
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
	failoverPtr   atomic.Pointer[failoverConfig]     // swapped on failover config changes
	config        configState                        // what the above were last loaded from; see RefreshConfig
	router        *routing.Router
	keyCache      *lookupcache.Cache[authedKey] // API keys by hash; nil = uncached
	now           func() time.Time
	pgSeenUp      atomic.Bool  // a /readyz ping has reached Postgres at least once
	pgDownSince   atomic.Int64 // unix nanos of the first failed /readyz ping; 0 = up
	draining      atomic.Bool  // shutting down: /readyz fails, requests are still served
	limiter       *limits.Limiter
	pricing       *pricing.Table
	sealer        *secrets.Sealer
	ledger        *ledger.Ledger
	auth          auth.Authenticator
	login         auth.LoginProvider
	loginLimiter  *auth.LoginLimiter
	oidc          oidcHandler
	httpc         *http.Client      // shared client for the DLP model sidecar
	capturePl     *capture.Pipeline // nil when capture is not configured
	blobStore     blob.Store        // nil when blob store is not configured
	captureIdx    captureReader     // nil until first audit route access (set in NewServer)
	metrics       *metrics.Metrics
	modelPool     *modelpool.Pool
	catalog       catalogCache     // per-provider upstream model list micro-cache
	breaker       *breaker.Breaker // per-tier circuit breaker; nil admits everything
	unavail       *unavail.Store   // per-(provider,model) Retry-After-aware skip; nil Check never skips
	affinity      *affinity.Store  // session pins to a backup tier; nil pins nothing

	// Test hooks: non-nil values replace the real implementations in tests.
	auditHook    func(ctx context.Context, actor, action, target string, detail any)
	ensureUserFn func(ctx context.Context, p auth.Principal) (ensuredUser, error)
}

// NewServer builds the routed handler.
func NewServer(cfg *config.Config, st *store.Store, deps Deps) *Server {
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	cacheOpts := func(name string) lookupcache.Options {
		return lookupcache.Options{Name: name, TTL: cfg.LookupCacheTTL, MaxStale: cfg.LookupCacheMaxStale, Now: now}
	}
	s := &Server{
		cfg:          cfg,
		st:           st,
		mux:          http.NewServeMux(),
		router:       routing.NewCachedRouter(st, cacheOpts("aliases")),
		keyCache:     lookupcache.New[authedKey](cacheOpts("api keys")),
		now:          now,
		limiter:      deps.Limiter,
		pricing:      deps.Pricing,
		sealer:       deps.Sealer,
		ledger:       ledger.New(st),
		auth:         deps.Auth,
		login:        deps.Login,
		loginLimiter: auth.NewLoginLimiter(st.RDB),
		oidc:         deps.OIDC,
		httpc:        &http.Client{},
		metrics:      metrics.New(),
	}
	if deps.Providers != nil {
		s.regPtr.Store(deps.Providers)
	} else {
		s.regPtr.Store(providers.NewRegistry()) // filled by ReloadProviders
	}
	s.loadDLP(context.Background())
	s.modelPool = modelpool.New(func() ([]string, int) {
		c := s.dlpCfg()
		return c.effectiveModelURLs(), c.ModelMaxConcurrency
	}, net.LookupHost)
	s.metrics.RegisterModelInflight(func() float64 { return float64(s.modelPool.Inflight()) })
	s.metrics.RegisterModelEndpoints(func() float64 { return float64(s.modelPool.Size()) })
	s.loadCapture(context.Background())
	s.loadSecondpass(context.Background())
	s.loadFailover(context.Background())
	s.breaker = s.newBreaker(st.RDB)
	s.unavail = unavail.New(st.RDB)
	s.affinity = affinity.New(st.RDB)
	s.metrics.RegisterBreakerStates(s.breakerStates)
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

// SetDraining makes /readyz fail from now on, while every other route keeps
// serving — called when shutdown starts, so a load balancer that routes by
// readiness moves new requests to the other replicas.
func (s *Server) SetDraining() { s.draining.Store(true) }

// Metrics exposes the server's metrics for wiring external gauge sources in main.
func (s *Server) Metrics() *metrics.Metrics { return s.metrics }

// Ledger exposes the server's ledger so main can drain it on graceful
// shutdown and wire its dropped-record gauge.
func (s *Server) Ledger() *ledger.Ledger { return s.ledger }

// StartModelPool kicks off the DLP model pool's resolver (initial + periodic
// re-resolve) until ctx is cancelled.
func (s *Server) StartModelPool(ctx context.Context) { s.modelPool.Start(ctx) }

// reg returns the current provider registry.
func (s *Server) reg() *providers.Registry { return s.regPtr.Load() }

// maxRequestBody caps request bodies to bound memory. It is generous enough
// for large prompts but blocks pathological payloads.
const maxRequestBody = 16 << 20 // 16 MiB

// maxAudioRequestBody caps audio upload bodies. Matches the 32 MiB
// ParseMultipartForm allows in api_audio.go — audio files routinely exceed
// the 16 MiB text-oriented maxRequestBody.
const maxAudioRequestBody = 32 << 20 // 32 MiB

// maxBodyFor returns the body-size cap for a request path.
func maxBodyFor(path string) int64 {
	switch path {
	case "/v1/audio/transcriptions", "/v1/audio/speech":
		return maxAudioRequestBody
	default:
		return maxRequestBody
	}
}

// statusRecorder captures the response status for metrics.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Flush forwards to the underlying ResponseWriter so SSE streaming handlers
// that type-assert http.Flusher keep working through the metrics wrapper.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// ingressOf maps a request path to a metrics ingress label.
func ingressOf(path string) string {
	switch path {
	case "/v1/chat/completions", "/v1/models", "/v1/audio/transcriptions", "/v1/audio/speech":
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
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyFor(r.URL.Path))
	}
	start := time.Now()
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	s.mux.ServeHTTP(rec, r.WithContext(withClientSession(r.Context(), r)))
	if r.Method != http.MethodGet && r.Method != http.MethodHead && strings.HasPrefix(r.URL.Path, "/api/") && rec.status/100 == 2 {
		// Any successful control-plane write may have revoked a key, disabled
		// a user, changed a role's policy, or edited an alias or a provider.
		// Rather than track which, forget every cached lookup so the change
		// applies on this instance at once; other instances catch up within
		// the TTL. Only successes count: a write that failed changed nothing,
		// and during an outage every write fails, so the cache the outage is
		// being served from is never thrown away by one.
		s.purgeLookupCaches()
	}
	switch r.URL.Path {
	case "/metrics", "/healthz", "/readyz":
		// infra endpoints — don't pollute request metrics
	default:
		s.metrics.RecordRequest(ingressOf(r.URL.Path), rec.status, time.Since(start))
	}
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", s.handleHealth)
	s.mux.HandleFunc("GET /readyz", s.handleReady)
	s.mux.Handle("GET /metrics", s.metrics.Handler())

	// Data-plane (API-key auth).
	s.mux.HandleFunc("POST /v1/chat/completions", s.requireAPIKey(s.handleChatCompletions))
	s.mux.HandleFunc("GET /v1/models", s.requireAPIKey(s.handleModels))
	s.mux.HandleFunc("POST /v1/messages", s.requireAPIKey(s.handleMessages))
	s.mux.HandleFunc("POST /v1/audio/transcriptions", s.requireAPIKey(s.handleAudioTranscriptions))
	s.mux.HandleFunc("POST /v1/audio/speech", s.requireAPIKey(s.handleAudioSpeech))

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
	s.mux.HandleFunc("GET /api/usage/series", s.requireSession(s.handleUsageSeries))
	s.mux.HandleFunc("GET /api/usage/breakdown", s.requireSession(s.handleUsageBreakdown))
	s.mux.HandleFunc("POST /api/me/password", s.requireSession(s.handleChangeOwnPassword))

	s.adminRoutes()
	s.auditRoutes()

	// Static SPA (catch-all GET; API prefixes excluded inside).
	s.registerSPA()
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// purgeLookupCaches forgets every cached API key and alias plan.
func (s *Server) purgeLookupCaches() {
	s.keyCache.Purge()
	if s.router != nil {
		s.router.PurgeCache()
	}
}

// handleReady reports the pod ready while it can serve. A Postgres outage
// shorter than the lookup caches' max staleness does not count: known keys
// and aliases are still served from memory, and taking every replica out of
// the Service would turn a database restart into a full outage. A pod that
// has never reached Postgres has nothing cached and gets no such grace.
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	if s.draining.Load() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "draining"})
		return
	}
	if err := s.st.PG.Ping(r.Context()); err != nil {
		now := s.now().UnixNano()
		s.pgDownSince.CompareAndSwap(0, now)
		if !s.pgSeenUp.Load() || time.Duration(now-s.pgDownSince.Load()) >= s.cfg.LookupCacheMaxStale {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "postgres unavailable"})
			return
		}
	} else {
		s.pgSeenUp.Store(true)
		s.pgDownSince.Store(0)
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
