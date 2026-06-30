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
	p.resolved.Store(true)
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
// ErrNoEndpoints when the pool has no resolved endpoints and ErrAllBusy when
// every endpoint is at capacity — both fail open. The pool lazily resolves
// once if it has never resolved (so a never-Started pool still works); a
// Start()-ed pool never resolves on this hot path. Any sidecar error is
// returned to the caller (which fails open).
func (p *Pool) Scan(ctx context.Context, hc *http.Client, content string, minScore float64) ([]dlp.Finding, error) {
	if !p.resolved.Load() {
		p.Resolve()
	}
	e, ok := p.pick()
	if !ok {
		if p.Size() == 0 {
			return nil, ErrNoEndpoints
		}
		return nil, ErrAllBusy
	}
	defer e.release()
	return dlp.ModelScan(ctx, hc, e.baseURL, minScore, content)
}
