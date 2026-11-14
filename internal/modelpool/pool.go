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
	"sync"
	"sync/atomic"
	"time"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/dlp"
)

// ErrAllBusy means every endpoint is at its concurrency cap. The caller should
// fail open (skip the model scan; the deterministic layer still runs).
var ErrAllBusy = errors.New("modelpool: all endpoints busy")

// ErrNoEndpoints means the pool has no resolved endpoints (e.g. the configured
// hostname does not resolve yet). Like ErrAllBusy the caller fails open, but it
// is a distinct reason: scaling up the pool will not help — fix DNS/config.
var ErrNoEndpoints = errors.New("modelpool: no resolved endpoints")

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

	eps      atomic.Pointer[[]*endpoint]
	rr       atomic.Uint64
	resolved atomic.Bool // set true after the first Resolve (initial or lazy)

	// lazyResolve guards the Scan hot path's lazy Resolve call (see Scan):
	// without it, several concurrent Scans racing a never-Started pool
	// before resolved flips true would each kick off their own independent
	// Resolve (duplicate DNS lookups + endpoint-slice rebuilds). A
	// Start()-ed pool never touches this — resolved is already true by the
	// time any Scan runs, so the outer check short-circuits first.
	lazyResolve sync.Once
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
		} else {
			got, resolveErr := p.resolve(host)
			switch {
			case resolveErr == nil && len(got) > 0:
				ips = got
			case resolveErr == nil:
				// A clean answer with zero addresses — e.g. a headless Service
				// scaled to zero backing pods — means the host genuinely has no
				// endpoints right now, not a transient lookup failure. Drop them
				// rather than keep stale/reassignable pod IPs around: Scan must
				// see ErrNoEndpoints, the one signal that actually says "this
				// won't fix itself by waiting," not a generic connect error.
				continue
			default:
				var dnsErr *net.DNSError
				if errors.As(resolveErr, &dnsErr) && dnsErr.IsNotFound {
					// NXDOMAIN: same "genuinely no endpoints" case as above, just
					// surfaced as a lookup error instead of an empty answer.
					continue
				}
				// A real transient resolver error (timeout, SERVFAIL, network
				// blip) — keep last-good endpoints so this doesn't empty the
				// pool over a blip the next resolve will likely clear.
				for _, e := range cur {
					if e.host == host {
						add(e)
					}
				}
				continue
			}
		}
		for _, ip := range ips {
			h := ip
			if port != "" {
				h = net.JoinHostPort(ip, port)
			} else if strings.Contains(ip, ":") {
				h = "[" + ip + "]" // bracket bare IPv6
			}
			base := u.Scheme + "://" + h + u.Path
			if e, ok := byURL[base]; ok && e.maxConc == maxConc {
				add(e)
			} else {
				add(newEndpoint(base, host, maxConc))
			}
		}
	}
	p.eps.Store(&next)
	p.resolved.Store(true)
}

// pick returns an acquired endpoint using round-robin, or false if all are at
// capacity. The caller MUST release the returned endpoint.
func (p *Pool) pick() (*endpoint, bool) {
	return p.pickExcluding(nil)
}

// pickExcluding is pick, skipping skip (used for Scan's one-shot retry so a
// retry never lands back on the endpoint that just failed). A nil skip
// behaves exactly like pick.
func (p *Pool) pickExcluding(skip *endpoint) (*endpoint, bool) {
	eps := p.load()
	n := len(eps)
	if n == 0 {
		return nil, false
	}
	start := int(p.rr.Add(1) % uint64(n))
	for i := 0; i < n; i++ {
		e := eps[(start+i)%n]
		if e == skip {
			continue
		}
		if e.acquire() {
			return e, true
		}
	}
	return nil, false
}

// Scan picks a free endpoint and runs a sidecar scan against it. It returns
// ErrNoEndpoints when the pool has no resolved endpoints and ErrAllBusy when
// every endpoint is at capacity — both fail open. The pool lazily resolves
// once if it has never resolved (so a never-Started pool still works); a
// Start()-ed pool never resolves on this hot path. truncated (meaningful only
// when err is nil) reports whether the sidecar scanned only a prefix of
// content — see dlp.ModelScan.
//
// A scan error gets one retry against a different endpoint (if one exists and
// the caller's context isn't already done) before failing open. Without this,
// one bad pod (OOMing, crash-looping, wrong model loaded) permanently eats its
// round-robin share of every scan — with N endpoints, 1/N of all requests
// silently lose BERT coverage, indistinguishable from a healthy occasional
// error.
func (p *Pool) Scan(ctx context.Context, hc *http.Client, content string, minScore float64) (findings []dlp.Finding, truncated bool, err error) {
	if !p.resolved.Load() {
		p.lazyResolve.Do(p.Resolve)
	}
	e, ok := p.pick()
	if !ok {
		if p.Size() == 0 {
			return nil, false, ErrNoEndpoints
		}
		return nil, false, ErrAllBusy
	}
	findings, truncated, err = dlp.ModelScan(ctx, hc, e.baseURL, minScore, content)
	e.release()
	if err != nil && ctx.Err() == nil {
		if e2, ok := p.pickExcluding(e); ok {
			findings, truncated, err = dlp.ModelScan(ctx, hc, e2.baseURL, minScore, content)
			e2.release()
		}
	}
	return findings, truncated, err
}
