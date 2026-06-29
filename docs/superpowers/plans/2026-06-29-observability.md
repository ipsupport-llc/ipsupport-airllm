# Observability (P2) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Expose Prometheus `/metrics` (request rate + per-component latency + tokens/cost + BERT load + saturation), ship Grafana dashboards as code, and add dependency-free in-console usage/latency sparklines.

**Architecture:** A small `internal/metrics` package wraps `prometheus/client_golang` on its own registry with nil-safe helper methods. A global middleware in `ServeHTTP` records request count/duration; per-stage spans in the data-plane handlers record component latency; `finalizeUsage` records tokens/cost; the two 429 sites and the BERT `ModelScan` call record saturation/load. The console reads a new hourly-bucket query over `usage_ledger` (which already stores `latency_ms`) and renders hand-rolled SVG sparklines. Grafana dashboards + a `metrics` compose profile complete the stack.

**Tech Stack:** Go 1.26, `github.com/prometheus/client_golang`, Postgres (`percentile_cont`), vanilla SVG, Prometheus + Grafana (compose profile).

## Global Constraints

- Module `github.com/ipsupport-llc/ipsupport-airllm`; Go 1.26.
- All metric names prefixed `airllm_`. Metrics live on a dedicated `*prometheus.Registry` (not the global default).
- Metric helper methods are **nil-safe** (`if m == nil { return }`) so Servers built without metrics (tests) never panic.
- `/metrics` is unauthenticated (mirrors `/healthz`/`/readyz`), documented internal-scrape only.
- In-console graphs are dependency-free hand-rolled SVG — NO new browser dependency, NO build step (`web/static/app.js` is `go:embed`-served).
- Public-clean: no secrets/hostnames/IPs/datasource UIDs in the repo; the Grafana dashboard datasource is a variable; compose Prometheus/Grafana are dev-only with placeholder creds.
- `go test -race ./...`, `go vet`, `gofmt -l` clean. English-only.
- Commit trailer: `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`.

## File Structure

- `internal/metrics/metrics.go` (create) — registry + collectors + nil-safe helpers + `Handler()`.
- `internal/httpapi/server.go` (modify) — `metrics` field; build in `NewServer`; `GET /metrics` route; `ServeHTTP` middleware + `statusRecorder` + `ingressOf`.
- `internal/httpapi/dataplane.go`, `messages.go` (modify) — per-stage spans + `usage_limit` 429 counter.
- `internal/httpapi/exec.go` (modify) — `finalizeUsage` token/cost; `runChat`/`runStream` `provider_busy` counter.
- `internal/httpapi/dlp.go` (modify) — BERT inflight/duration around `ModelScan`.
- `internal/httpapi/api_self.go` (modify) — `usageSeries` query + `handleUsageSeries`; `clampHours` helper.
- `internal/httpapi/api_admin.go` (modify) — `handleAdminUsageSeries`; route.
- `cmd/ipsupport-airllm/main.go` (modify) — register the capture-dropped gauge source.
- `web/static/app.js` (modify) — `sparkline()` + Dashboard sparklines.
- `deploy/prometheus/prometheus.yml`, `deploy/grafana/**` (create), `deploy/docker-compose.yml` (modify) — `metrics` profile.
- Docs: `docs/configuration.md`, `docs/operations.md`, `docs/architecture.md` (modify).

---

### Task 1: `internal/metrics` package

**Files:**
- Create: `internal/metrics/metrics.go`
- Test: `internal/metrics/metrics_test.go`

**Interfaces:**
- Produces: `metrics.New() *Metrics`; `(*Metrics).Handler() http.Handler`; nil-safe `RecordRequest(ingress string, status int, d time.Duration)`, `ObserveComponent(component string, d time.Duration)`, `RecordUsage(ingress string, prompt, completion int, cost float64)`, `IncRateLimited(reason string)`, `DLPModelInc()`, `DLPModelDone(d time.Duration)`, `RegisterCaptureDropped(fn func() float64)`.

- [ ] **Step 1: Add the dependency**

```bash
go get github.com/prometheus/client_golang/prometheus github.com/prometheus/client_golang/prometheus/promhttp
go mod tidy
```

- [ ] **Step 2: Write the failing test** — `internal/metrics/metrics_test.go`

```go
package metrics

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestNewRegistersAndRecords(t *testing.T) {
	m := New()
	m.RecordRequest("openai", 200, 12*time.Millisecond)
	m.RecordRequest("openai", 200, 8*time.Millisecond)
	m.ObserveComponent("provider", 5*time.Millisecond)
	m.RecordUsage("openai", 10, 20, 0.001)
	m.IncRateLimited("usage_limit")
	m.DLPModelInc()
	m.DLPModelDone(3 * time.Millisecond)

	if got := testutil.ToFloat64(m.httpRequests.WithLabelValues("openai", "200")); got != 2 {
		t.Errorf("http_requests_total{openai,200} = %v, want 2", got)
	}
	if got := testutil.ToFloat64(m.tokens.WithLabelValues("openai", "prompt")); got != 10 {
		t.Errorf("tokens prompt = %v, want 10", got)
	}
	if got := testutil.ToFloat64(m.tokens.WithLabelValues("openai", "completion")); got != 20 {
		t.Errorf("tokens completion = %v, want 20", got)
	}
	if got := testutil.ToFloat64(m.rateLimited.WithLabelValues("usage_limit")); got != 1 {
		t.Errorf("rate_limited{usage_limit} = %v, want 1", got)
	}
}

func TestNilSafe(t *testing.T) {
	var m *Metrics // nil
	// Must not panic.
	m.RecordRequest("openai", 200, time.Millisecond)
	m.ObserveComponent("dlp", time.Millisecond)
	m.RecordUsage("anthropic", 1, 1, 0.0)
	m.IncRateLimited("provider_busy")
	m.DLPModelInc()
	m.DLPModelDone(time.Millisecond)
}

func TestRegisterCaptureDropped(t *testing.T) {
	m := New()
	m.RegisterCaptureDropped(func() float64 { return 7 })
	got, err := testutil.GatherAndCount(m.reg, "airllm_capture_dropped")
	if err != nil || got != 1 {
		t.Fatalf("capture_dropped gauge not registered: count=%d err=%v", got, err)
	}
}
```

- [ ] **Step 3: Run to verify it fails**

Run: `go test ./internal/metrics/`
Expected: FAIL (`New` undefined / package empty).

- [ ] **Step 4: Implement `internal/metrics/metrics.go`**

```go
// Package metrics exposes Prometheus instrumentation for the gateway on a
// dedicated registry. All helper methods are nil-safe so a Server built without
// metrics (in tests) never panics.
package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics holds the gateway's collectors and their registry.
type Metrics struct {
	reg          *prometheus.Registry
	httpRequests *prometheus.CounterVec
	httpDuration *prometheus.HistogramVec
	component    *prometheus.HistogramVec
	tokens       *prometheus.CounterVec
	cost         *prometheus.CounterVec
	rateLimited  *prometheus.CounterVec
	dlpInflight  prometheus.Gauge
	dlpDuration  prometheus.Histogram
}

// New builds and registers the collectors on a fresh registry.
func New() *Metrics {
	m := &Metrics{
		reg: prometheus.NewRegistry(),
		httpRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "airllm_http_requests_total", Help: "Total HTTP requests by ingress and status.",
		}, []string{"ingress", "status"}),
		httpDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "airllm_http_request_duration_seconds", Help: "HTTP request duration by ingress.",
			Buckets: prometheus.DefBuckets,
		}, []string{"ingress"}),
		component: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "airllm_component_duration_seconds", Help: "Per-component latency.",
			Buckets: prometheus.DefBuckets,
		}, []string{"component"}),
		tokens: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "airllm_tokens_total", Help: "Tokens metered by ingress and direction.",
		}, []string{"ingress", "direction"}),
		cost: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "airllm_cost_usd_total", Help: "Cost in USD metered by ingress.",
		}, []string{"ingress"}),
		rateLimited: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "airllm_rate_limited_total", Help: "Requests rejected with 429 by reason.",
		}, []string{"reason"}),
		dlpInflight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "airllm_dlp_model_requests_inflight", Help: "In-flight DLP model (BERT) scans.",
		}),
		dlpDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "airllm_dlp_model_duration_seconds", Help: "DLP model (BERT) scan duration.",
			Buckets: prometheus.DefBuckets,
		}),
	}
	m.reg.MustRegister(m.httpRequests, m.httpDuration, m.component, m.tokens, m.cost, m.rateLimited, m.dlpInflight, m.dlpDuration)
	return m
}

// Handler serves the registry in the Prometheus text format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}

func (m *Metrics) RecordRequest(ingress string, status int, d time.Duration) {
	if m == nil {
		return
	}
	m.httpRequests.WithLabelValues(ingress, strconv.Itoa(status)).Inc()
	m.httpDuration.WithLabelValues(ingress).Observe(d.Seconds())
}

func (m *Metrics) ObserveComponent(component string, d time.Duration) {
	if m == nil {
		return
	}
	m.component.WithLabelValues(component).Observe(d.Seconds())
}

func (m *Metrics) RecordUsage(ingress string, prompt, completion int, cost float64) {
	if m == nil {
		return
	}
	m.tokens.WithLabelValues(ingress, "prompt").Add(float64(prompt))
	m.tokens.WithLabelValues(ingress, "completion").Add(float64(completion))
	m.cost.WithLabelValues(ingress).Add(cost)
}

func (m *Metrics) IncRateLimited(reason string) {
	if m == nil {
		return
	}
	m.rateLimited.WithLabelValues(reason).Inc()
}

func (m *Metrics) DLPModelInc() {
	if m == nil {
		return
	}
	m.dlpInflight.Inc()
}

func (m *Metrics) DLPModelDone(d time.Duration) {
	if m == nil {
		return
	}
	m.dlpInflight.Dec()
	m.dlpDuration.Observe(d.Seconds())
}

// RegisterCaptureDropped registers a gauge that reads the capture pipeline's
// cumulative dropped count from fn.
func (m *Metrics) RegisterCaptureDropped(fn func() float64) {
	if m == nil {
		return
	}
	m.reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "airllm_capture_dropped", Help: "Capture records dropped due to a full buffer.",
	}, fn))
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test -race ./internal/metrics/ && go vet ./internal/metrics/`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/metrics/ go.mod go.sum
git commit -m "feat(metrics): prometheus collectors package (nil-safe helpers)"
```

---

### Task 2: wire metrics into Server + /metrics + global request middleware

**Files:**
- Modify: `internal/httpapi/server.go`
- Test: `internal/httpapi/metrics_wire_test.go`

**Interfaces:**
- Consumes: `metrics.New()` / `(*Metrics)` helpers (Task 1).
- Produces: `Server.metrics *metrics.Metrics` (accessible to handlers); `GET /metrics`; `ingressOf(path string) string`.

- [ ] **Step 1: Write the failing test** — `internal/httpapi/metrics_wire_test.go`

```go
package httpapi

import "testing"

func TestIngressOf(t *testing.T) {
	cases := map[string]string{
		"/v1/chat/completions": "openai",
		"/v1/models":           "openai",
		"/v1/messages":         "anthropic",
		"/api/usage":           "control",
		"/healthz":             "control",
	}
	for path, want := range cases {
		if got := ingressOf(path); got != want {
			t.Errorf("ingressOf(%q) = %q, want %q", path, got, want)
		}
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/httpapi/ -run TestIngressOf`
Expected: FAIL (`ingressOf` undefined).

- [ ] **Step 3: Add the field, build it, register the route, wrap ServeHTTP** — `internal/httpapi/server.go`

Add the import `"github.com/ipsupport-llc/ipsupport-airllm/internal/metrics"`, add the field to `Server`:

```go
	metrics       *metrics.Metrics
```

In `NewServer`, set it in the struct literal (before `s.routes()` runs):

```go
		metrics: metrics.New(),
```

In `routes()`, next to `/healthz`:

```go
	s.mux.Handle("GET /metrics", s.metrics.Handler())
```

Replace `ServeHTTP` with a status-capturing, timing middleware:

```go
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
```

(Ensure `"time"` is imported in `server.go`.)

- [ ] **Step 4: Run tests + build**

Run: `go test ./internal/httpapi/ -run TestIngressOf && go build ./... && go vet ./...`
Expected: PASS, build OK. (Existing httpapi tests build `&Server{}` without metrics; the nil-safe helpers keep them green.)

- [ ] **Step 5: Commit**

```bash
git add internal/httpapi/server.go internal/httpapi/metrics_wire_test.go
git commit -m "feat(metrics): /metrics endpoint + request middleware"
```

---

### Task 3: per-component spans + tokens/cost + 429 counters

**Files:**
- Modify: `internal/httpapi/dataplane.go`, `internal/httpapi/messages.go`, `internal/httpapi/exec.go`

**Interfaces:**
- Consumes: `s.metrics.ObserveComponent`, `RecordUsage`, `IncRateLimited` (Task 1/2).

- [ ] **Step 1: Time the stages in `handleChatCompletions`** — `internal/httpapi/dataplane.go`

Wrap each stage. Replace the existing stage calls with timed versions:

```go
	t0 := time.Now()
	plan, err := s.router.Resolve(r.Context(), req.Model, ak.Policy.AllowPassthrough)
	s.metrics.ObserveComponent("routing", time.Since(t0))
	if err != nil {
		writeProtocolError(w, r, http.StatusNotFound, "invalid_request_error", err.Error())
		return
	}
	t0 = time.Now()
	if msg, denied := s.limitDenied(r.Context(), ak); denied {
		s.metrics.ObserveComponent("limits", time.Since(t0))
		s.metrics.IncRateLimited("usage_limit")
		writeProtocolError(w, r, http.StatusTooManyRequests, "rate_limit_error", msg)
		return
	}
	s.metrics.ObserveComponent("limits", time.Since(t0))
	t0 = time.Now()
	blocked, msg, dlpRes := s.dlpEnforce(r.Context(), ak, "openai", &req)
	s.metrics.ObserveComponent("dlp", time.Since(t0))
	if blocked {
		writeProtocolError(w, r, http.StatusBadRequest, "invalid_request_error", msg)
		return
	}
```

And time the provider call (non-stream) — wrap `runChat`:

```go
	tp := time.Now()
	resp, target, callErr := s.runChat(r.Context(), plan, req)
	s.metrics.ObserveComponent("provider", time.Since(tp))
```

- [ ] **Step 2: Mirror the same spans in `handleMessages`** — `internal/httpapi/messages.go`

Apply the identical pattern (router/limits/dlp spans + `usage_limit` counter at the limit branch; `provider` span around `runChat`). Use ingress `"anthropic"` in the `dlpEnforce` call (already present).

- [ ] **Step 3: Record tokens/cost in `finalizeUsage`** — `internal/httpapi/exec.go`

At the end of `finalizeUsage`, after `s.ledger.Record(ctx, entry)`:

```go
	s.metrics.RecordUsage(entry.IngressProtocol, prompt, completion, entry.CostUSD)
```

- [ ] **Step 4: Count provider-busy 429 at the source** — `internal/httpapi/exec.go`

In `runChat`, where it returns `errAllBusy` (the final return after the retry loop), and in `runStream` at its `errAllBusy` return, add immediately before the return:

```go
	s.metrics.IncRateLimited("provider_busy")
	return ..., errAllBusy   // (existing return)
```

(Both `runChat` and `runStream` are `*Server` methods, so `s.metrics` is in scope.)

- [ ] **Step 5: Build + vet + gofmt**

Run: `go build ./... && go vet ./... && gofmt -l internal/httpapi/`
Expected: clean. (No new unit tests — these are instrumentation side-effects on existing paths; the controller live-verifies `/metrics` shows component/token/429 series moving after traffic.)

- [ ] **Step 6: Commit**

```bash
git add internal/httpapi/dataplane.go internal/httpapi/messages.go internal/httpapi/exec.go
git commit -m "feat(metrics): per-component latency, token/cost, 429 counters"
```

---

### Task 4: BERT load metrics + capture-dropped gauge

**Files:**
- Modify: `internal/httpapi/dlp.go`, `cmd/ipsupport-airllm/main.go`

**Interfaces:**
- Consumes: `s.metrics.DLPModelInc/DLPModelDone`, `apiSrv.metrics.RegisterCaptureDropped`, `capturePipeline.Dropped()`.

- [ ] **Step 1: Wrap the ModelScan call** — `internal/httpapi/dlp.go`

The call site (inside the per-message loop) is:

```go
			mctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			mf, err := dlp.ModelScan(mctx, s.httpc, cfg.ModelURL, cfg.ModelMinScore, content)
			cancel()
```

Wrap it with inflight + duration:

```go
			mctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			mstart := time.Now()
			s.metrics.DLPModelInc()
			mf, err := dlp.ModelScan(mctx, s.httpc, cfg.ModelURL, cfg.ModelMinScore, content)
			s.metrics.DLPModelDone(time.Since(mstart))
			cancel()
```

(`"time"` is already imported in `dlp.go`.)

- [ ] **Step 2: Register the capture-dropped gauge source** — `cmd/ipsupport-airllm/main.go`

After `apiSrv := httpapi.NewServer(cfg, st, deps)` and the `capturePipeline` is in scope, expose a method to register the source. First add to `internal/httpapi/server.go`:

```go
// Metrics exposes the server's metrics for wiring external gauge sources in main.
func (s *Server) Metrics() *metrics.Metrics { return s.metrics }
```

Then in `main.go`, after `apiSrvPtr.Store(apiSrv)`:

```go
	apiSrv.Metrics().RegisterCaptureDropped(func() float64 { return float64(capturePipeline.Dropped()) })
```

- [ ] **Step 3: Build + vet + gofmt**

Run: `go build ./... && go vet ./... && gofmt -l internal/ cmd/`
Expected: clean. (Live-verified: with the BERT sidecar on, traffic moves `airllm_dlp_model_requests_inflight`/`_duration_seconds`; `airllm_capture_dropped` is exposed.)

- [ ] **Step 4: Commit**

```bash
git add internal/httpapi/dlp.go internal/httpapi/server.go cmd/ipsupport-airllm/main.go
git commit -m "feat(metrics): BERT inflight/duration + capture-dropped gauge"
```

---

### Task 5: usage-series query + endpoints

**Files:**
- Modify: `internal/httpapi/api_self.go`, `internal/httpapi/api_admin.go`, `internal/httpapi/server.go`
- Test: `internal/httpapi/series_test.go`

**Interfaces:**
- Produces: `GET /api/usage/series` (session), `GET /api/admin/usage/series` (admin), both `{"series":[{ts,requests,tokens,cost_usd,p50_ms,p95_ms}…]}`; `clampHours(raw string) int`.

- [ ] **Step 1: Write the failing test** — `internal/httpapi/series_test.go`

```go
package httpapi

import "testing"

func TestClampHours(t *testing.T) {
	cases := map[string]int{
		"":     24,  // default
		"0":    24,  // non-positive -> default
		"-5":   24,
		"48":   48,
		"999":  168, // capped at 7 days
		"abc":  24,  // unparseable -> default
	}
	for in, want := range cases {
		if got := clampHours(in); got != want {
			t.Errorf("clampHours(%q) = %d, want %d", in, got, want)
		}
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/httpapi/ -run TestClampHours`
Expected: FAIL (`clampHours` undefined).

- [ ] **Step 3: Implement the query, handlers, and helper** — `internal/httpapi/api_self.go`

```go
import "strconv" // add if not present

type seriesPoint struct {
	Ts       time.Time `json:"ts"`
	Requests int64     `json:"requests"`
	Tokens   int64     `json:"tokens"`
	CostUSD  float64   `json:"cost_usd"`
	P50ms    int64     `json:"p50_ms"`
	P95ms    int64     `json:"p95_ms"`
}

// clampHours parses the ?hours param, defaulting to 24 and capping at 168 (7d).
func clampHours(raw string) int {
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 24
	}
	if n > 168 {
		return 168
	}
	return n
}

// usageSeries returns hourly usage buckets over the last `hours`. where is an
// optional "WHERE user_id = $2" (args[0] is the hours interval; args[1...] are
// the where args).
func (s *Server) usageSeries(ctx context.Context, where string, hours int, whereArgs ...any) ([]seriesPoint, error) {
	args := append([]any{hours}, whereArgs...)
	q := `
		SELECT date_trunc('hour', ts) AS bucket,
		       count(*),
		       COALESCE(SUM(prompt_tokens + completion_tokens), 0),
		       COALESCE(SUM(cost_usd), 0),
		       COALESCE(percentile_cont(0.5)  WITHIN GROUP (ORDER BY latency_ms), 0)::bigint,
		       COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY latency_ms), 0)::bigint
		FROM usage_ledger
		WHERE ts > now() - make_interval(hours => $1) ` + where + `
		GROUP BY 1 ORDER BY 1`
	rows, err := s.st.PG.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []seriesPoint{}
	for rows.Next() {
		var p seriesPoint
		if err := rows.Scan(&p.Ts, &p.Requests, &p.Tokens, &p.CostUSD, &p.P50ms, &p.P95ms); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// handleUsageSeries returns the caller's hourly usage buckets.
func (s *Server) handleUsageSeries(w http.ResponseWriter, r *http.Request) {
	sess, _ := sessionFrom(r.Context())
	hours := clampHours(r.URL.Query().Get("hours"))
	series, err := s.usageSeries(r.Context(), `AND user_id = $2`, hours, sess.userID)
	if err != nil {
		writeControlError(w, http.StatusInternalServerError, "failed to load usage series")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"series": series})
}
```

- [ ] **Step 4: Admin global handler** — `internal/httpapi/api_admin.go`

```go
// handleAdminUsageSeries returns global hourly usage buckets.
func (s *Server) handleAdminUsageSeries(w http.ResponseWriter, r *http.Request) {
	hours := clampHours(r.URL.Query().Get("hours"))
	series, err := s.usageSeries(r.Context(), ``, hours)
	if err != nil {
		writeControlError(w, http.StatusInternalServerError, "failed to load usage series")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"series": series})
}
```

- [ ] **Step 5: Register routes**

`internal/httpapi/server.go` (control-plane, near `/api/usage`):

```go
	s.mux.HandleFunc("GET /api/usage/series", s.requireSession(s.handleUsageSeries))
```

`internal/httpapi/api_admin.go` admin registrar (`a := s.requireAdmin`):

```go
	s.mux.HandleFunc("GET /api/admin/usage/series", a(s.handleAdminUsageSeries))
```

- [ ] **Step 6: Run test + build**

Run: `go test ./internal/httpapi/ -run TestClampHours && go build ./... && go vet ./...`
Expected: PASS, build OK. (The SQL is live-verified by the controller hitting `/api/usage/series` after seeding traffic.)

- [ ] **Step 7: Commit**

```bash
git add internal/httpapi/api_self.go internal/httpapi/api_admin.go internal/httpapi/server.go internal/httpapi/series_test.go
git commit -m "feat(usage): hourly usage-series query + endpoints"
```

---

### Task 6: in-console sparklines

**Files:**
- Modify: `web/static/app.js`

**Interfaces:**
- Consumes: `GET /api/usage/series` (or `/api/admin/usage/series` for admins), `{series:[{ts,requests,tokens,cost_usd,p50_ms,p95_ms}]}`.

- [ ] **Step 1: Add a dependency-free SVG sparkline helper**

Near the other helpers (e.g. by `fmtNum`) in `web/static/app.js`:

```js
// sparkline renders an inline SVG polyline from numeric values. No deps.
function sparkline(values, opts) {
  const o = Object.assign({ w: 220, h: 40, stroke: "var(--accent)" }, opts || {});
  const vals = (values && values.length) ? values : [0];
  const max = Math.max(...vals, 1), min = Math.min(...vals, 0);
  const span = (max - min) || 1;
  const step = vals.length > 1 ? o.w / (vals.length - 1) : o.w;
  const pts = vals.map((v, i) => `${(i * step).toFixed(1)},${(o.h - ((v - min) / span) * o.h).toFixed(1)}`).join(" ");
  return `<svg viewBox="0 0 ${o.w} ${o.h}" width="100%" height="${o.h}" preserveAspectRatio="none" style="display:block">
    <polyline fill="none" stroke="${o.stroke}" stroke-width="2" points="${pts}" /></svg>`;
}
```

- [ ] **Step 2: Render sparklines on the Dashboard**

In the Dashboard render (where the usage cards are built), after fetching `/api/me`, fetch the series (admin path when `me.is_admin`) and add a panel of four sparklines under the cards:

```js
const seriesPath = me.is_admin ? "/api/admin/usage/series" : "/api/usage/series";
const sr = await api("GET", seriesPath);
const series = (sr.data && sr.data.series) || [];
const spark = (label, key, fmt) => {
  const vals = series.map((p) => Number(p[key]) || 0);
  const last = vals.length ? vals[vals.length - 1] : 0;
  return `<div class="card"><div class="sub">${label} · 24h</div>
    <div class="value">${fmt(last)}</div>${sparkline(vals)}</div>`;
};
const sparksHtml = series.length
  ? `<div class="cards" style="margin-top:1rem">
      ${spark("Tokens/hr", "tokens", fmtNum)}
      ${spark("Cost/hr", "cost_usd", (v) => "$" + Number(v).toFixed(4))}
      ${spark("Requests/hr", "requests", fmtNum)}
      ${spark("p95 latency", "p95_ms", (v) => fmtNum(v) + " ms")}
    </div>`
  : `<div class="empty" style="margin-top:1rem">No usage history yet.</div>`;
```

Insert `sparksHtml` into the Dashboard markup after the existing usage cards. (Reuse the existing `.card`/`.cards`/`.value`/`.sub`/`.empty` classes and `esc`/`fmtNum` helpers; introduce no new dependency.)

- [ ] **Step 3: Syntax check + commit**

Run: `node --check web/static/app.js`
Expected: OK. (Controller live-verifies by screenshotting the Dashboard with traffic present.)

```bash
git add web/static/app.js
git commit -m "feat(console): dependency-free usage sparklines on the dashboard"
```

---

### Task 7: Grafana dashboards-as-code + Prometheus + compose `metrics` profile

**Files:**
- Create: `deploy/prometheus/prometheus.yml`, `deploy/grafana/provisioning/datasources/prometheus.yml`, `deploy/grafana/provisioning/dashboards/dashboards.yml`, `deploy/grafana/dashboards/airllm-overview.json`
- Modify: `deploy/docker-compose.yml`

- [ ] **Step 1: Prometheus scrape config** — `deploy/prometheus/prometheus.yml`

```yaml
global:
  scrape_interval: 15s
scrape_configs:
  - job_name: airllm
    static_configs:
      - targets: ["app:8080"]
```

- [ ] **Step 2: Grafana provisioning**

`deploy/grafana/provisioning/datasources/prometheus.yml`:

```yaml
apiVersion: 1
datasources:
  - name: Prometheus
    uid: airllm-prometheus
    type: prometheus
    access: proxy
    url: http://prometheus:9090
    isDefault: true
```

`deploy/grafana/provisioning/dashboards/dashboards.yml`:

```yaml
apiVersion: 1
providers:
  - name: airllm
    type: file
    options:
      path: /var/lib/grafana/dashboards
```

- [ ] **Step 3: The dashboard** — `deploy/grafana/dashboards/airllm-overview.json`

A Grafana dashboard JSON whose datasource is the templated variable `${DS_PROMETHEUS}` (a `datasource`-type templating variable of type `prometheus`), with these panels (timeseries unless noted), each using the panel's `datasource: ${DS_PROMETHEUS}`:

1. **Request rate by status** — `sum by (status) (rate(airllm_http_requests_total[5m]))`
2. **Request latency p50/p95** — `histogram_quantile(0.5, sum by (le) (rate(airllm_http_request_duration_seconds_bucket[5m])))` and the 0.95 variant.
3. **Per-component latency p95** — `histogram_quantile(0.95, sum by (le,component) (rate(airllm_component_duration_seconds_bucket[5m])))`
4. **Tokens rate** — `sum by (direction) (rate(airllm_tokens_total[5m]))`; **Cost rate** — `sum(rate(airllm_cost_usd_total[5m]))`
5. **Rate-limited** — `sum by (reason) (rate(airllm_rate_limited_total[5m]))`
6. **BERT in-flight** (stat/timeseries) — `airllm_dlp_model_requests_inflight`; **BERT scan p95** — `histogram_quantile(0.95, sum by (le) (rate(airllm_dlp_model_duration_seconds_bucket[5m])))`

Write valid Grafana dashboard JSON (schemaVersion ~39, `templating.list` with the `DS_PROMETHEUS` datasource variable, `title: "AirLLM Overview"`, `uid: "airllm-overview"`). No hardcoded datasource UID in the panels — they reference `${DS_PROMETHEUS}`. Verify it parses: `python3 -c "import json;json.load(open('deploy/grafana/dashboards/airllm-overview.json'))"`.

- [ ] **Step 4: Compose `metrics` profile** — `deploy/docker-compose.yml`

Add two services (mirroring the `dlp-bert` profile shape), loopback-bound:

```yaml
  prometheus:
    image: prom/prometheus:v2.54.1
    profiles: ["metrics"]
    volumes:
      - ./prometheus/prometheus.yml:/etc/prometheus/prometheus.yml:ro
    ports:
      - "127.0.0.1:9090:9090"

  grafana:
    image: grafana/grafana:11.2.0
    profiles: ["metrics"]
    environment:
      GF_SECURITY_ADMIN_PASSWORD: "admin"   # dev-only; change on any real deploy
      GF_USERS_ALLOW_SIGN_UP: "false"
    volumes:
      - ./grafana/provisioning:/etc/grafana/provisioning:ro
      - ./grafana/dashboards:/var/lib/grafana/dashboards:ro
    ports:
      - "127.0.0.1:3000:3000"
    depends_on: [prometheus]
```

- [ ] **Step 5: Verify the YAML/JSON parse + commit**

Run: `docker compose -f deploy/docker-compose.yml config >/dev/null && echo compose-ok` and the JSON parse from Step 3.
(Controller live-verifies: `docker compose --profile metrics up -d`, then Prometheus target `up==1` and Grafana loads the provisioned dashboard.)

```bash
git add deploy/prometheus/ deploy/grafana/ deploy/docker-compose.yml
git commit -m "feat(deploy): metrics compose profile + Grafana dashboard-as-code"
```

---

### Task 8: docs

**Files:**
- Modify: `docs/configuration.md`, `docs/operations.md`, `docs/architecture.md`

- [ ] **Step 1: configuration.md** — document the `/metrics` endpoint (unauthenticated; internal-scrape only), the compose `metrics` profile (`docker compose --profile metrics up`, Prometheus `127.0.0.1:9090`, Grafana `127.0.0.1:3000`), and `GF_SECURITY_ADMIN_PASSWORD` (dev default `admin`).

- [ ] **Step 2: operations.md** — observability section: scrape `/metrics` internally (never via the public ingress); the metric catalog (the `airllm_*` series); dashboards-as-code in `deploy/grafana/` (datasource is a variable; import into any Grafana); the in-console sparklines come from `usage_ledger`.

- [ ] **Step 3: architecture.md** — add an "Observability" subsection: Prometheus `/metrics` + per-component spans + BERT load; console sparklines from the ledger; Grafana for deep graphs.

- [ ] **Step 4: Verify + commit**

Run: `grep -rn "/metrics\|--profile metrics\|airllm_" docs/` shows the additions.

```bash
git add docs/
git commit -m "docs: observability — /metrics, metrics profile, dashboards, sparklines"
```

---

## Notes for the executor

- The project has no DB unit-test harness; the `usageSeries` SQL and all instrumentation side-effects are **live-verified** against the compose stack (same split as the capture store / P1 user store). Pure logic (metrics helpers, `ingressOf`, `clampHours`) is unit-tested.
- Metric helpers are nil-safe so the existing `&Server{}` handler tests stay green without wiring metrics.
- After Task 5 the headline live check is: send some data-plane traffic, then `curl /metrics` shows `airllm_*` series moving and `GET /api/usage/series` returns hourly buckets.
- Run `go test -race ./...`, `go vet ./...`, `gofmt -l` before each commit.
