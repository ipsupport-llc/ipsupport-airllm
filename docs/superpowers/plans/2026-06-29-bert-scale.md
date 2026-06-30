# BERT-scale (P3) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the single-URL DLP BERT sidecar call with a balanced, self-resolving pool of endpoints so the model layer scales in docker-compose (explicit list **and** `--scale`) and kubernetes (Service + replicas + HPA), failing open when saturated.

**Architecture:** A new `internal/modelpool` package holds a round-robin pool of endpoints, each with a non-blocking concurrency slot mirroring `providers.Entry`. A configured hostname is resolved to its A-records (one endpoint per IP); an explicit URL list yields one endpoint per URL. A 30s background re-resolver keeps the set fresh. `dlpEnforce` calls the pool instead of `dlp.ModelScan` directly; an all-busy pool returns `ErrAllBusy` and the scan is skipped (deterministic layer still redacts). The in-flight gauge and a new endpoints gauge are sourced from the pool as `GaugeFunc`s (same pattern as `capture_dropped`); a new `skipped_total` counter records saturation.

**Tech Stack:** Go 1.26, stdlib `net`/`net/url`/`sync/atomic`, `prometheus/client_golang`, embedded vanilla JS SPA.

## Global Constraints

- **Public-clean:** no real hostnames, IPs, secrets, emails, or org-specific values committed. Service names (`dlp-bert`), placeholders (`http://dlp-bert:8000`), and admin-set runtime settings only.
- **English-only:** no Russian anywhere in the repo (code, comments, docs, UI copy).
- **Loopback binding:** any compose host port binds `127.0.0.1` only.
- **Back-compat:** the existing single `model_url` setting keeps working as a fallback when `model_urls` is empty.
- **Green gate:** `gofmt -l` clean, `go build ./...` and `go test ./...` pass before every commit.
- **Surgical:** every changed line traces to this plan. Match surrounding style. Do not refactor unrelated code.
- **Module path:** `github.com/ipsupport-llc/ipsupport-airllm`.
- **Implementers do NOT run `docker compose`.** Logic is unit-tested with fakes/httptest; compose `--scale` distribution and the gauges are live-verified by the controller.

---

## File Structure

- `internal/modelpool/pool.go` (create) — `Pool`, `endpoint`, `acquire/release`, `pick`, `Resolve`, `Start`, `Scan`, `Inflight`, `Size`, `ErrAllBusy`.
- `internal/modelpool/pool_test.go` (create) — pool logic with a fake resolver + httptest.
- `internal/httpapi/dlp.go` (modify) — add `ModelURLs`/`ModelMaxConcurrency` fields + `effectiveModelURLs()`; `dlpEnforce` uses the pool.
- `internal/httpapi/dlp_modelurls_test.go` (create) — `effectiveModelURLs` unit tests.
- `internal/metrics/metrics.go` (modify) — drop manual inflight gauge; add `DLPModelObserve`, `DLPModelSkipped`, `RegisterModelInflight`, `RegisterModelEndpoints`.
- `internal/metrics/metrics_test.go` (modify) — track the new metrics API.
- `internal/httpapi/server.go` (modify) — `modelPool` field, build + register gauges in `NewServer`, `StartModelPool`.
- `cmd/ipsupport-airllm/main.go` (modify) — call `apiSrv.StartModelPool(ctx)`.
- `web/static/app.js` (modify) — DLP tab: sidecar URL list + per-endpoint concurrency inputs.
- `deploy/docker-compose.yml` (modify) — document both scaling modes in the `dlp-bert` comment.
- `docs/configuration.md`, `docs/operations.md`, `docs/dlp-capture-audit.md` (modify) — config keys + scaling runbook.

---

### Task 1: `internal/modelpool` package

**Files:**
- Create: `internal/modelpool/pool.go`
- Test: `internal/modelpool/pool_test.go`

**Interfaces:**
- Consumes: `dlp.ModelScan(ctx, *http.Client, baseURL string, minScore float64, text string) ([]dlp.Finding, error)` and `dlp.Finding` from `internal/dlp` (unchanged).
- Produces:
  - `modelpool.New(cfgFn func() (urls []string, maxConc int), resolveFn func(host string) ([]string, error)) *Pool`
  - `(*Pool).Start(ctx context.Context)` — initial resolve + 30s re-resolver until ctx done
  - `(*Pool).Resolve()` — rebuild endpoint set
  - `(*Pool).Scan(ctx context.Context, hc *http.Client, content string, minScore float64) ([]dlp.Finding, error)` — returns `ErrAllBusy` when saturated
  - `(*Pool).Size() int`, `(*Pool).Inflight() int64`
  - `var ErrAllBusy error`

- [ ] **Step 1: Write the failing test**

Create `internal/modelpool/pool_test.go`:

```go
package modelpool

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
)

// cfg returns a fixed cfgFn for tests.
func cfg(urls []string, maxConc int) func() ([]string, int) {
	return func() ([]string, int) { return urls, maxConc }
}

func baseURLs(p *Pool) []string {
	var out []string
	for _, e := range p.load() {
		out = append(out, e.baseURL)
	}
	sort.Strings(out)
	return out
}

func TestResolveHostnameFansOut(t *testing.T) {
	resolve := func(host string) ([]string, error) {
		if host != "bert" {
			t.Fatalf("unexpected host %q", host)
		}
		return []string{"10.0.0.1", "10.0.0.2"}, nil
	}
	p := New(cfg([]string{"http://bert:8000"}, 0), resolve)
	p.Resolve()
	got := baseURLs(p)
	want := []string{"http://10.0.0.1:8000", "http://10.0.0.2:8000"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("baseURLs = %v, want %v", got, want)
	}
}

func TestResolveExplicitList(t *testing.T) {
	resolve := func(host string) ([]string, error) {
		switch host {
		case "a":
			return []string{"1.1.1.1"}, nil
		case "b":
			return []string{"2.2.2.2"}, nil
		}
		return nil, errors.New("no")
	}
	p := New(cfg([]string{"http://a:8000", "http://b:8000"}, 0), resolve)
	p.Resolve()
	if got := p.Size(); got != 2 {
		t.Fatalf("Size = %d, want 2", got)
	}
}

func TestResolveIPLiteralSkipsDNS(t *testing.T) {
	resolve := func(host string) ([]string, error) {
		t.Fatalf("resolver called for IP literal host %q", host)
		return nil, nil
	}
	p := New(cfg([]string{"http://127.0.0.1:8000"}, 0), resolve)
	p.Resolve()
	if got := baseURLs(p); len(got) != 1 || got[0] != "http://127.0.0.1:8000" {
		t.Fatalf("baseURLs = %v, want [http://127.0.0.1:8000]", got)
	}
}

func TestResolveKeepsLastGoodOnFailure(t *testing.T) {
	ok := true
	resolve := func(host string) ([]string, error) {
		if ok {
			return []string{"10.0.0.1", "10.0.0.2"}, nil
		}
		return nil, errors.New("dns down")
	}
	p := New(cfg([]string{"http://bert:8000"}, 0), resolve)
	p.Resolve()
	if p.Size() != 2 {
		t.Fatalf("after good resolve Size = %d, want 2", p.Size())
	}
	ok = false
	p.Resolve()
	if p.Size() != 2 {
		t.Fatalf("after failed resolve Size = %d, want 2 (last-good kept)", p.Size())
	}
}

func TestPickRoundRobinAndCap(t *testing.T) {
	resolve := func(string) ([]string, error) { return []string{"1.1.1.1", "2.2.2.2"}, nil }
	p := New(cfg([]string{"http://bert:8000"}, 1), resolve)
	p.Resolve()

	e1, ok1 := p.pick()
	e2, ok2 := p.pick()
	if !ok1 || !ok2 {
		t.Fatalf("first two picks should succeed: %v %v", ok1, ok2)
	}
	if e1 == e2 {
		t.Fatalf("round-robin returned the same endpoint twice")
	}
	if _, ok3 := p.pick(); ok3 {
		t.Fatalf("third pick should fail (all at cap 1)")
	}
	e1.release()
	if _, ok4 := p.pick(); !ok4 {
		t.Fatalf("pick should succeed after a release")
	}
}

func TestScanAllBusy(t *testing.T) {
	resolve := func(string) ([]string, error) { return []string{"1.1.1.1"}, nil }
	p := New(cfg([]string{"http://bert:8000"}, 1), resolve)
	p.Resolve()
	held, ok := p.pick() // occupy the only slot
	if !ok {
		t.Fatal("pick should succeed")
	}
	defer held.release()
	if _, err := p.Scan(context.Background(), http.DefaultClient, "hi", 0.5); !errors.Is(err, ErrAllBusy) {
		t.Fatalf("Scan err = %v, want ErrAllBusy", err)
	}
}

func TestScanHappyPathAndInflightReleased(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"findings":[{"label":"PER","start":0,"end":3,"score":0.9}]}`))
	}))
	defer srv.Close()
	// srv.URL host is 127.0.0.1 (IP literal) → resolver not exercised.
	p := New(cfg([]string{srv.URL}, 0), func(string) ([]string, error) { return nil, errors.New("no") })
	p.Resolve()
	fs, err := p.Scan(context.Background(), srv.Client(), "Bob", 0.5)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(fs) != 1 || fs[0].Label != "pii:PER" {
		t.Fatalf("findings = %+v, want one pii:PER", fs)
	}
	if n := p.Inflight(); n != 0 {
		t.Fatalf("Inflight = %d after Scan, want 0", n)
	}
}

func TestScanLazyResolvesWhenEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"findings":[]}`))
	}))
	defer srv.Close()
	p := New(cfg([]string{srv.URL}, 0), func(string) ([]string, error) { return nil, errors.New("no") })
	// No Resolve() call — Scan must lazily resolve.
	if _, err := p.Scan(context.Background(), srv.Client(), "x", 0.5); err != nil {
		t.Fatalf("Scan (lazy resolve): %v", err)
	}
	if p.Size() == 0 {
		t.Fatalf("Scan did not lazily resolve")
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/modelpool/`
Expected: FAIL — package/symbols undefined (`New`, `Pool`, `ErrAllBusy`, …).

- [ ] **Step 3: Write the implementation**

Create `internal/modelpool/pool.go`:

```go
// Package modelpool balances DLP BERT-NER sidecar scans across a set of
// resolved endpoints. It mirrors the providers.Entry concurrency primitive:
// each endpoint has a non-blocking acquire/release slot, and the pool
// round-robins across endpoints, skipping busy ones. A single configured
// hostname is resolved to its A-records so `docker compose --scale` and k8s
// headless Services fan out to one endpoint per backing IP.
package modelpool

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/dlp"
)

// ErrAllBusy means every endpoint is at its concurrency cap. The caller should
// fail open (skip the model scan; the deterministic layer still runs).
var ErrAllBusy = errors.New("modelpool: all endpoints busy")

// resolveInterval is how often Start re-resolves configured hostnames so
// scaled-up/down replicas are picked up without a restart.
const resolveInterval = 30 * time.Second

// endpoint is one concrete sidecar URL with a concurrency limit. host is the
// configured hostname it was resolved from (for last-good retention).
type endpoint struct {
	baseURL  string
	host     string
	sem      chan struct{} // nil = unlimited
	maxConc  int
	inflight atomic.Int64
}

func newEndpoint(baseURL, host string, maxConc int) *endpoint {
	e := &endpoint{baseURL: baseURL, host: host, maxConc: maxConc}
	if maxConc > 0 {
		e.sem = make(chan struct{}, maxConc)
	}
	return e
}

// acquire tries to take a slot without blocking.
func (e *endpoint) acquire() bool {
	if e.sem == nil {
		e.inflight.Add(1)
		return true
	}
	select {
	case e.sem <- struct{}{}:
		e.inflight.Add(1)
		return true
	default:
		return false
	}
}

// release returns a previously acquired slot.
func (e *endpoint) release() {
	e.inflight.Add(-1)
	if e.sem != nil {
		<-e.sem
	}
}

// Pool holds an atomically-swapped set of endpoints plus a round-robin cursor.
type Pool struct {
	cfgFn   func() (urls []string, maxConc int)
	resolve func(host string) ([]string, error)

	eps atomic.Pointer[[]*endpoint]
	rr  atomic.Uint64
}

// New builds a pool. cfgFn supplies the live endpoint URLs and per-endpoint
// concurrency cap (read on every resolve). resolveFn resolves a hostname to
// IPs; pass net.LookupHost in production (a fake in tests).
func New(cfgFn func() ([]string, int), resolveFn func(string) ([]string, error)) *Pool {
	p := &Pool{cfgFn: cfgFn, resolve: resolveFn}
	empty := []*endpoint{}
	p.eps.Store(&empty)
	return p
}

func (p *Pool) load() []*endpoint { return *p.eps.Load() }

// Size reports the current resolved endpoint count.
func (p *Pool) Size() int { return len(p.load()) }

// Inflight reports the total in-flight scans across all endpoints.
func (p *Pool) Inflight() int64 {
	var n int64
	for _, e := range p.load() {
		n += e.inflight.Load()
	}
	return n
}

// Start runs an initial resolve, then re-resolves every resolveInterval until
// ctx is cancelled.
func (p *Pool) Start(ctx context.Context) {
	p.Resolve()
	go func() {
		t := time.NewTicker(resolveInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				p.Resolve()
			}
		}
	}()
}

// Resolve rebuilds the endpoint set from the configured URLs. Each URL's host
// is resolved to IPs (one endpoint per IP); an IP literal yields one endpoint.
// Existing endpoints are reused (preserving inflight/sem state) when the
// baseURL and cap are unchanged. If a hostname fails to resolve, its last-good
// endpoints are kept so a transient DNS error never empties the pool.
func (p *Pool) Resolve() {
	urls, maxConc := p.cfgFn()
	cur := p.load()
	byURL := make(map[string]*endpoint, len(cur))
	for _, e := range cur {
		byURL[e.baseURL] = e
	}
	var next []*endpoint
	seen := make(map[string]bool)
	add := func(e *endpoint) {
		if !seen[e.baseURL] {
			seen[e.baseURL] = true
			next = append(next, e)
		}
	}
	for _, raw := range urls {
		u, err := url.Parse(raw)
		if err != nil || u.Hostname() == "" {
			continue
		}
		host, port := u.Hostname(), u.Port()
		var ips []string
		if net.ParseIP(host) != nil {
			ips = []string{host}
		} else if got, err := p.resolve(host); err == nil && len(got) > 0 {
			ips = got
		} else {
			// keep last-good endpoints for this host
			for _, e := range cur {
				if e.host == host {
					add(e)
				}
			}
			continue
		}
		for _, ip := range ips {
			h := ip
			if port != "" {
				h = net.JoinHostPort(ip, port)
			} else if strings.Contains(ip, ":") {
				h = "[" + ip + "]" // bracket bare IPv6
			}
			base := u.Scheme + "://" + h
			if e, ok := byURL[base]; ok && e.maxConc == maxConc {
				add(e)
			} else {
				add(newEndpoint(base, host, maxConc))
			}
		}
	}
	p.eps.Store(&next)
}

// pick returns an acquired endpoint using round-robin, or false if all are at
// capacity. The caller MUST release the returned endpoint.
func (p *Pool) pick() (*endpoint, bool) {
	eps := p.load()
	n := len(eps)
	if n == 0 {
		return nil, false
	}
	start := int(p.rr.Add(1) % uint64(n))
	for i := 0; i < n; i++ {
		e := eps[(start+i)%n]
		if e.acquire() {
			return e, true
		}
	}
	return nil, false
}

// Scan picks a free endpoint and runs a sidecar scan against it. It returns
// ErrAllBusy when every endpoint is at capacity. If the pool is empty it
// lazily resolves once, so a never-Started pool still works. Any sidecar error
// is returned to the caller (which fails open).
func (p *Pool) Scan(ctx context.Context, hc *http.Client, content string, minScore float64) ([]dlp.Finding, error) {
	if p.Size() == 0 {
		p.Resolve()
	}
	e, ok := p.pick()
	if !ok {
		return nil, ErrAllBusy
	}
	defer e.release()
	return dlp.ModelScan(ctx, hc, e.baseURL, minScore, content)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `gofmt -l internal/modelpool/ && go test ./internal/modelpool/`
Expected: no gofmt output; PASS (all tests).

- [ ] **Step 5: Commit**

```bash
git add internal/modelpool/
git commit -m "feat(dlp): add balanced BERT sidecar pool (modelpool)"
```

---

### Task 2: DLP config fields + `effectiveModelURLs`

**Files:**
- Modify: `internal/httpapi/dlp.go` (struct `dlpConfig` ~line 30-33; add a method)
- Test: `internal/httpapi/dlp_modelurls_test.go` (create)

**Interfaces:**
- Produces: `dlpConfig.ModelURLs []string`, `dlpConfig.ModelMaxConcurrency int`, and `(dlpConfig).effectiveModelURLs() []string` — returns `ModelURLs` (blanks trimmed/dropped), falling back to `[ModelURL]` when empty.
- Consumes: nothing new.

- [ ] **Step 1: Write the failing test**

Create `internal/httpapi/dlp_modelurls_test.go`:

```go
package httpapi

import "testing"

func TestEffectiveModelURLs(t *testing.T) {
	cases := []struct {
		name string
		cfg  dlpConfig
		want []string
	}{
		{"list wins", dlpConfig{ModelURLs: []string{"http://a:8000", "http://b:8000"}, ModelURL: "http://old:8000"}, []string{"http://a:8000", "http://b:8000"}},
		{"fallback to single", dlpConfig{ModelURL: "http://old:8000"}, []string{"http://old:8000"}},
		{"blanks dropped", dlpConfig{ModelURLs: []string{" ", "http://a:8000", ""}}, []string{"http://a:8000"}},
		{"all empty list falls back", dlpConfig{ModelURLs: []string{"", "  "}, ModelURL: "http://old:8000"}, []string{"http://old:8000"}},
		{"nothing", dlpConfig{}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.cfg.effectiveModelURLs()
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/httpapi/ -run TestEffectiveModelURLs`
Expected: FAIL — `ModelURLs`/`ModelMaxConcurrency`/`effectiveModelURLs` undefined.

- [ ] **Step 3: Add the fields**

In `internal/httpapi/dlp.go`, extend the `dlpConfig` struct's model block (after `ModelMinScore float64`):

```go
	// Layer 2: optional BERT-NER sidecar for fuzzy/contextual PII.
	ModelEnabled  bool    `json:"model_enabled"`
	ModelURL      string  `json:"model_url"`
	ModelMinScore float64 `json:"model_min_score"`
	// ModelURLs is the sidecar endpoint list; when empty the single ModelURL is
	// used (back-compat). ModelMaxConcurrency caps concurrent scans per endpoint
	// (0 = unlimited). See internal/modelpool.
	ModelURLs           []string `json:"model_urls,omitempty"`
	ModelMaxConcurrency int      `json:"model_max_concurrency,omitempty"`
```

- [ ] **Step 4: Add the method**

In `internal/httpapi/dlp.go`, add after `defaultDLPConfig` (or near the other `dlpConfig` helpers):

```go
// effectiveModelURLs returns the configured sidecar URLs, falling back to the
// single ModelURL for back-compat. Blank entries are dropped.
func (c dlpConfig) effectiveModelURLs() []string {
	var out []string
	for _, u := range c.ModelURLs {
		if u = strings.TrimSpace(u); u != "" {
			out = append(out, u)
		}
	}
	if len(out) == 0 {
		if u := strings.TrimSpace(c.ModelURL); u != "" {
			out = append(out, u)
		}
	}
	return out
}
```

(`strings` is already imported in `dlp.go`.)

- [ ] **Step 5: Run tests to verify they pass**

Run: `gofmt -l internal/httpapi/dlp.go && go test ./internal/httpapi/ -run TestEffectiveModelURLs`
Expected: no gofmt output; PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/httpapi/dlp.go internal/httpapi/dlp_modelurls_test.go
git commit -m "feat(dlp): add model_urls + model_max_concurrency config"
```

---

### Task 3: metrics — pool-sourced gauges, skipped counter

**Files:**
- Modify: `internal/metrics/metrics.go`
- Modify: `internal/metrics/metrics_test.go`

**Interfaces:**
- Produces:
  - `(*Metrics).DLPModelObserve(d time.Duration)` — observes scan duration (replaces `DLPModelDone`)
  - `(*Metrics).DLPModelSkipped(reason string)` — increments `airllm_dlp_model_skipped_total{reason}`
  - `(*Metrics).RegisterModelInflight(fn func() float64)` — `GaugeFunc` `airllm_dlp_model_requests_inflight`
  - `(*Metrics).RegisterModelEndpoints(fn func() float64)` — `GaugeFunc` `airllm_dlp_model_endpoints`
- Removes: `DLPModelInc`, `DLPModelDone`, and the manually-set `dlpInflight` gauge field. (Grep confirmed only `dlp.go` and `metrics_test.go` reference them; both are updated in this plan.)

- [ ] **Step 1: Update the test to the new API (write the failing test)**

In `internal/metrics/metrics_test.go`:

Replace the two lines in `TestNewRegistersAndRecords`:
```go
	m.DLPModelInc()
	m.DLPModelDone(3 * time.Millisecond)
```
with:
```go
	m.DLPModelObserve(3 * time.Millisecond)
	m.DLPModelSkipped("all_busy")
```

Add this assertion at the end of `TestNewRegistersAndRecords` (before the closing brace):
```go
	if got := testutil.ToFloat64(m.dlpSkipped.WithLabelValues("all_busy")); got != 1 {
		t.Errorf("dlp_model_skipped_total{all_busy} = %v, want 1", got)
	}
```

Replace the two lines in `TestNilSafe`:
```go
	m.DLPModelInc()
	m.DLPModelDone(time.Millisecond)
```
with:
```go
	m.DLPModelObserve(time.Millisecond)
	m.DLPModelSkipped("all_busy")
	m.RegisterModelInflight(func() float64 { return 0 })
	m.RegisterModelEndpoints(func() float64 { return 0 })
```

Add a new test:
```go
func TestRegisterModelGauges(t *testing.T) {
	m := New()
	m.RegisterModelInflight(func() float64 { return 2 })
	m.RegisterModelEndpoints(func() float64 { return 3 })
	if got, err := testutil.GatherAndCount(m.reg, "airllm_dlp_model_requests_inflight"); err != nil || got != 1 {
		t.Fatalf("inflight gauge not registered: count=%d err=%v", got, err)
	}
	if got, err := testutil.GatherAndCount(m.reg, "airllm_dlp_model_endpoints"); err != nil || got != 1 {
		t.Fatalf("endpoints gauge not registered: count=%d err=%v", got, err)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/metrics/`
Expected: FAIL — `DLPModelObserve`/`DLPModelSkipped`/`RegisterModelInflight`/`RegisterModelEndpoints`/`dlpSkipped` undefined.

- [ ] **Step 3: Update `metrics.go`**

In the `Metrics` struct, replace `dlpInflight prometheus.Gauge` with `dlpSkipped *prometheus.CounterVec` (keep `dlpDuration prometheus.Histogram`):

```go
	dlpDuration *prometheus.Histogram // existing — keep as-is
	dlpSkipped  *prometheus.CounterVec
```
(Note: keep `dlpDuration`'s existing type; only swap the `dlpInflight` line for `dlpSkipped`.)

In `New()`, remove the `dlpInflight` initializer and add:
```go
		dlpSkipped: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "airllm_dlp_model_skipped_total", Help: "DLP model scans skipped by reason (e.g. all_busy).",
		}, []string{"reason"}),
```

In the `MustRegister(...)` call, replace `m.dlpInflight` with `m.dlpSkipped` (keep `m.dlpDuration`):
```go
	m.reg.MustRegister(m.httpRequests, m.httpDuration, m.component, m.tokens, m.cost, m.rateLimited, m.dlpSkipped, m.dlpDuration)
```

Remove `DLPModelInc` and `DLPModelDone`. Add:
```go
func (m *Metrics) DLPModelObserve(d time.Duration) {
	if m == nil {
		return
	}
	m.dlpDuration.Observe(d.Seconds())
}

func (m *Metrics) DLPModelSkipped(reason string) {
	if m == nil {
		return
	}
	m.dlpSkipped.WithLabelValues(reason).Inc()
}

// RegisterModelInflight registers a gauge reading the live in-flight DLP model
// (BERT) scan count from fn (sourced from the model pool).
func (m *Metrics) RegisterModelInflight(fn func() float64) {
	if m == nil {
		return
	}
	m.reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "airllm_dlp_model_requests_inflight", Help: "In-flight DLP model (BERT) scans across the pool.",
	}, fn))
}

// RegisterModelEndpoints registers a gauge reading the resolved sidecar
// endpoint count from fn.
func (m *Metrics) RegisterModelEndpoints(fn func() float64) {
	if m == nil {
		return
	}
	m.reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "airllm_dlp_model_endpoints", Help: "Resolved DLP model (BERT) sidecar endpoints in the pool.",
	}, fn))
}
```

> Note on the struct field type: the existing field is declared `dlpDuration prometheus.Histogram` (value type). Leave it exactly as it is — only the `dlpInflight` line changes to `dlpSkipped *prometheus.CounterVec`. Ignore the illustrative pointer in Step 3's struct snippet if it conflicts with the real declaration; match the file.

- [ ] **Step 4: Run tests to verify they pass**

Run: `gofmt -l internal/metrics/ && go test ./internal/metrics/`
Expected: no gofmt output; PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/metrics/
git commit -m "feat(metrics): pool-sourced BERT gauges + skipped counter"
```

---

### Task 4: server wiring + `dlpEnforce` integration

**Files:**
- Modify: `internal/httpapi/server.go` (struct field, `NewServer`, new method)
- Modify: `internal/httpapi/dlp.go` (`dlpEnforce` model block)
- Modify: `cmd/ipsupport-airllm/main.go` (start the pool)

**Interfaces:**
- Consumes: `modelpool.New`, `(*Pool).Start`, `(*Pool).Scan`, `(*Pool).Inflight`, `(*Pool).Size`, `modelpool.ErrAllBusy`; `dlpConfig.effectiveModelURLs`, `dlpConfig.ModelMaxConcurrency`; metrics `DLPModelObserve`/`DLPModelSkipped`/`RegisterModelInflight`/`RegisterModelEndpoints`.
- Produces: `(*Server).StartModelPool(ctx context.Context)`.

- [ ] **Step 1: Add the `modelPool` field + import**

In `internal/httpapi/server.go`, add to imports:
```go
	"net"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/modelpool"
```
Add the field to `Server` (near `metrics *metrics.Metrics`):
```go
	modelPool *modelpool.Pool
```

- [ ] **Step 2: Build the pool + register gauges in `NewServer`**

In `NewServer`, after `s.loadDLP(context.Background())` (so `dlpCfg` is populated) and before `s.routes()`, add:
```go
	s.modelPool = modelpool.New(func() ([]string, int) {
		c := s.dlpCfg()
		return c.effectiveModelURLs(), c.ModelMaxConcurrency
	}, net.LookupHost)
	s.metrics.RegisterModelInflight(func() float64 { return float64(s.modelPool.Inflight()) })
	s.metrics.RegisterModelEndpoints(func() float64 { return float64(s.modelPool.Size()) })
```

- [ ] **Step 3: Add `StartModelPool`**

In `internal/httpapi/server.go`, add near `Metrics()`:
```go
// StartModelPool kicks off the DLP model pool's resolver (initial + periodic
// re-resolve) until ctx is cancelled.
func (s *Server) StartModelPool(ctx context.Context) { s.modelPool.Start(ctx) }
```

- [ ] **Step 4: Wire `dlpEnforce` to the pool**

In `internal/httpapi/dlp.go`:

Add imports `"errors"` and the modelpool package:
```go
	"errors"
	...
	"github.com/ipsupport-llc/ipsupport-airllm/internal/modelpool"
```

In `dlpEnforce`, just before the `for i := range req.Messages {` loop, hoist the gate:
```go
	modelOn := cfg.ModelEnabled && len(cfg.effectiveModelURLs()) > 0
```

Replace the existing model block (the `if cfg.ModelEnabled && cfg.ModelURL != "" { … }`) with:
```go
		if modelOn {
			mctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			mstart := time.Now()
			mf, err := s.modelPool.Scan(mctx, s.httpc, content, cfg.ModelMinScore)
			cancel()
			switch {
			case errors.Is(err, modelpool.ErrAllBusy):
				s.metrics.DLPModelSkipped("all_busy")
			case err != nil:
				s.metrics.DLPModelObserve(time.Since(mstart))
				slog.Error("dlp model scan failed; deterministic layer only", "err", err)
			default:
				s.metrics.DLPModelObserve(time.Since(mstart))
				if mf = filterModelFindings(mf, cfg.Patterns); len(mf) > 0 {
					findings = dlp.Merge(append(findings, mf...))
				}
			}
		}
```

- [ ] **Step 5: Start the pool in `main`**

In `cmd/ipsupport-airllm/main.go`, immediately after:
```go
	apiSrv.Metrics().RegisterCaptureDropped(func() float64 { return float64(capturePipeline.Dropped()) })
```
add:
```go
	apiSrv.StartModelPool(ctx)
```

- [ ] **Step 6: Build + test**

Run: `gofmt -l internal/httpapi/ cmd/ && go build ./... && go test ./...`
Expected: no gofmt output; build OK; all packages PASS (including the pre-existing `internal/dlp/model_test.go`, which is untouched).

- [ ] **Step 7: Commit**

```bash
git add internal/httpapi/server.go internal/httpapi/dlp.go cmd/ipsupport-airllm/main.go
git commit -m "feat(dlp): route model scans through the balanced pool"
```

> **Controller live-verify (not the implementer):** `docker compose --profile bert up -d --build --scale dlp-bert=3`; set DLP `model_enabled=true`, Sidecar URL `http://dlp-bert:8000`; drive `/v1/chat/completions` traffic with PII; confirm `airllm_dlp_model_endpoints == 3` on `/metrics`, requests spread across the 3 replica logs, and a concurrency-capped burst increments `airllm_dlp_model_skipped_total{reason="all_busy"}` while responses still succeed (deterministic redaction intact).

---

### Task 5: console UI — sidecar URL list + concurrency

**Files:**
- Modify: `web/static/app.js` (DLP policy panel ~lines 1003-1009; `gatherDLP` ~lines 1060-1065)

**Interfaces:**
- Consumes: `model_urls` (array) and `model_max_concurrency` (int) from `GET /api/admin/dlp`; sends them back via `PUT /api/admin/dlp` in `gatherDLP`.

- [ ] **Step 1: Add the inputs**

In `web/static/app.js`, after the "Min score (0–1)" field (the `#dlp-mscore` label), add:
```js
        <label class="field"><span class="lab">Sidecar URLs (one per line — overrides the single URL above)</span>
          <textarea id="dlp-murls" rows="3" class="mono" placeholder="http://dlp-bert:8000">${esc((d.model_urls || []).join("\n"))}</textarea></label>
        <label class="field"><span class="lab">Max concurrent scans per endpoint (0 = unlimited)</span>
          <input id="dlp-mconc" type="number" min="0" value="${d.model_max_concurrency ?? 0}" /></label>
```

- [ ] **Step 2: Gather the new fields on save**

In `gatherDLP`, extend the `body` object (alongside `model_enabled`/`model_url`/`model_min_score`):
```js
      model_urls: $("#dlp-murls").value.split("\n").map((s) => s.trim()).filter(Boolean),
      model_max_concurrency: Number($("#dlp-mconc").value) || 0,
```

- [ ] **Step 3: Syntax-check the JS**

Run: `node --check web/static/app.js`
Expected: no output (valid).

- [ ] **Step 4: Commit**

```bash
git add web/static/app.js
git commit -m "feat(ui): DLP sidecar URL list + per-endpoint concurrency"
```

> **Controller live-verify:** open `/` → DLP tab; confirm the textarea + number input render in the dark theme, save round-trips (reload shows persisted values), and a multi-line list persists as a JSON array.

---

### Task 6: compose comment + docs

**Files:**
- Modify: `deploy/docker-compose.yml` (the `dlp-bert` service comment, ~lines 55-57)
- Modify: `docs/configuration.md` (DLP section — add the two keys)
- Modify: `docs/operations.md` (add a "Scaling the DLP BERT sidecar" runbook)
- Modify: `docs/dlp-capture-audit.md` (note the pool/scaling where the model layer is described, if present)

**Interfaces:** none (docs only).

- [ ] **Step 1: Document both scaling modes in compose**

In `deploy/docker-compose.yml`, replace the `dlp-bert` comment block (the three `#` lines above `dlp-bert:`) with:
```yaml
  # DLP BERT-NER sidecar (layer 2). Opt-in: `--profile bert`. Heavy image
  # (torch + model weights). Loopback-only on the host.
  #
  # Scale it two ways (configure under Admin → DLP in the console):
  #   * one URL, N replicas — `docker compose --profile bert up -d --scale dlp-bert=3`,
  #     then set Sidecar URL = http://dlp-bert:8000 (the pool resolves all 3 IPs).
  #   * explicit list — add more sidecar services and list each in "Sidecar URLs".
  # Set "Max concurrent scans per endpoint" to cap load per replica; a saturated
  # pool skips the model scan (deterministic redaction still applies).
```

- [ ] **Step 2: Document the config keys**

In `docs/configuration.md`, find the DLP `model_url` / `model_min_score` description and add adjacent entries for:
- `model_urls` — array of sidecar URLs; overrides `model_url` when non-empty; a single hostname is resolved to all its A-records (one pool endpoint per IP), so `docker compose --scale` and k8s Services fan out automatically.
- `model_max_concurrency` — per-endpoint cap on concurrent scans (0 = unlimited); when every endpoint is at the cap the scan is skipped and only the deterministic layer runs.

(Match the file's existing format — table row or list item, whichever it uses. Run `grep -n "model_url\|model_min_score" docs/configuration.md` to locate.)

- [ ] **Step 3: Add the scaling runbook**

In `docs/operations.md`, add a section "Scaling the DLP BERT sidecar":
- compose: `docker compose --profile bert up -d --scale dlp-bert=N` + set Sidecar URL to the service name; or list explicit URLs.
- k8s: a `dlp-bert` Deployment with `replicas: N` behind a Service (normal Service → kube-proxy load-balances; headless Service → one pool endpoint per pod), with an HPA on CPU. (The chart ships in P5.)
- Signals to watch on `/metrics`: `airllm_dlp_model_endpoints` (pool size), `airllm_dlp_model_requests_inflight` (load), `airllm_dlp_model_duration_seconds` (latency), and `airllm_dlp_model_skipped_total{reason="all_busy"}` (rising ⇒ scale up).

- [ ] **Step 4: Cross-reference the model layer doc**

Run `grep -n "model_url\|sidecar\|BERT" docs/dlp-capture-audit.md`. If the model layer is described there, add one sentence: the gateway balances scans across a pool of sidecar endpoints and fails open (skips the scan) when all endpoints are busy. If not present, skip this step.

- [ ] **Step 5: Commit**

```bash
git add deploy/docker-compose.yml docs/configuration.md docs/operations.md docs/dlp-capture-audit.md
git commit -m "docs(dlp): document BERT sidecar scaling (model_urls, --scale, k8s)"
```

---

## Self-Review

**Spec coverage:** pool + per-endpoint concurrency + round-robin + all-busy skip (Task 1), config `model_urls`/`model_max_concurrency` + back-compat (Task 2), `skipped_total` + endpoints/inflight gauges (Task 3), hot-path integration + lifecycle (Task 4), UI (Task 5), compose `--scale` + explicit list + k8s docs (Task 6). The "both" resolution requirement (URL list AND single-hostname A-record fan-out) is in Task 1 `Resolve`. All covered.

**Type consistency:** `New(cfgFn, resolveFn)`, `Scan(ctx, hc, content, minScore)`, `Start(ctx)`, `Size`/`Inflight`, `ErrAllBusy`, `effectiveModelURLs()`, `ModelMaxConcurrency`, `DLPModelObserve`/`DLPModelSkipped`/`RegisterModelInflight`/`RegisterModelEndpoints` are used identically across tasks.

**Placeholder scan:** none — every code step shows complete code; doc steps name the exact file/anchor and content.

**Note for the executor:** the Task 3 struct snippet illustratively shows `dlpDuration` as a pointer; the real field is a value type — match the file, only swap the `dlpInflight` line. Flagged inline in Task 3.
