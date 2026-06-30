package modelpool

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
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

func TestScanNoEndpoints(t *testing.T) {
	// Configured URL whose host never resolves → 0 endpoints. resolved=true after
	// Resolve, so Scan must NOT re-resolve on the hot path and must return ErrNoEndpoints.
	p := New(cfg([]string{"http://bert:8000"}, 0), func(string) ([]string, error) { return nil, errors.New("nxdomain") })
	p.Resolve()
	if _, err := p.Scan(context.Background(), http.DefaultClient, "x", 0.5); !errors.Is(err, ErrNoEndpoints) {
		t.Fatalf("Scan err = %v, want ErrNoEndpoints", err)
	}
}

func TestConcurrentPickReleaseAndResolve(t *testing.T) {
	resolve := func(string) ([]string, error) { return []string{"1.1.1.1", "2.2.2.2", "3.3.3.3"}, nil }
	p := New(cfg([]string{"http://bert:8000"}, 4), resolve)
	p.Resolve()
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 2000; i++ {
				if e, ok := p.pick(); ok {
					e.release()
				}
			}
		}()
	}
	for r := 0; r < 2; r++ { // concurrent re-resolvers swap the endpoint set
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				p.Resolve()
			}
		}()
	}
	wg.Wait()
	if n := p.Inflight(); n != 0 {
		t.Fatalf("Inflight = %d after concurrent pick/release, want 0", n)
	}
}
