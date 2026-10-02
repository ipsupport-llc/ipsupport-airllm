package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/auth"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/llm"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/providers"
)

// countingLister is a Provider+ModelLister that counts ListModels calls.
type countingLister struct {
	providers.Provider // embeds mock for Chat/ChatStream
	calls              int
	models             []string
	err                error
}

func (c *countingLister) ListModels(_ context.Context) ([]string, error) {
	c.calls++
	if c.err != nil {
		return nil, c.err
	}
	return c.models, nil
}

// bareProvider implements Provider but NOT ModelLister.
type bareProvider struct{ providers.Provider }

func (b *bareProvider) Name() string { return "bare" }

func newModelsTestServer(t *testing.T, reg *providers.Registry) *Server {
	t.Helper()
	s := &Server{
		mux:  http.NewServeMux(),
		auth: &fakeAuth{principal: auth.Principal{Subject: "a", Roles: []string{auth.AdminRole}}},
		ensureUserFn: func(_ context.Context, p auth.Principal) (ensuredUser, error) {
			return ensuredUser{id: "uid-" + p.Subject, roles: p.Roles}, nil
		},
	}
	s.regPtr.Store(reg)
	s.mux.HandleFunc("GET /api/admin/providers/{name}/models",
		s.requireAdmin(s.handleAdminProviderModels))
	return s
}

func getModels(t *testing.T, s *Server, name string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/admin/providers/"+name+"/models", nil)
	rw := httptest.NewRecorder()
	s.mux.ServeHTTP(rw, req)
	var body map[string]any
	json.Unmarshal(rw.Body.Bytes(), &body)
	return rw.Code, body
}

func TestProviderModelsSuccessAndCache(t *testing.T) {
	cl := &countingLister{Provider: providers.NewMock("up"), models: []string{"m-a", "m-b"}}
	reg := providers.NewRegistry()
	reg.Register(cl, 0)
	// countingLister embeds the mock whose Name() is "up".
	s := newModelsTestServer(t, reg)

	code, body := getModels(t, s, "up")
	if code != http.StatusOK {
		t.Fatalf("code = %d, want 200", code)
	}
	got, _ := body["models"].([]any)
	if len(got) != 2 {
		t.Fatalf("models = %v, want 2 entries", body["models"])
	}

	// Second call within TTL must be served from cache.
	getModels(t, s, "up")
	if cl.calls != 1 {
		t.Errorf("upstream calls = %d, want 1 (second call cached)", cl.calls)
	}
}

// blockingLister blocks every ListModels call on release, so a test can
// hold several concurrent requests in flight at once before letting any of
// them complete.
type blockingLister struct {
	providers.Provider
	calls   int32
	release chan struct{}
	models  []string
}

func (b *blockingLister) ListModels(_ context.Context) ([]string, error) {
	atomic.AddInt32(&b.calls, 1)
	<-b.release
	return b.models, nil
}

// TestProviderModelsConcurrentRequestsDedupe is the Admin API Minor fix: the
// catalog cache had no single-flight lock, so N concurrent requests racing
// an empty (or just-expired) cache each independently called the upstream.
// Holds 5 concurrent requests in flight against a lister that blocks until
// released, then asserts the upstream was called exactly once.
func TestProviderModelsConcurrentRequestsDedupe(t *testing.T) {
	bl := &blockingLister{Provider: providers.NewMock("up"), release: make(chan struct{}), models: []string{"m-a"}}
	reg := providers.NewRegistry()
	reg.Register(bl, 0)
	s := newModelsTestServer(t, reg)

	const n = 5
	var wg sync.WaitGroup
	codes := make([]int, n)
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			codes[i], _ = getModels(t, s, "up")
		}(i)
	}
	time.Sleep(100 * time.Millisecond) // let every goroutine reach ListModels (or block waiting on the leader)
	close(bl.release)
	wg.Wait()

	for i, c := range codes {
		if c != http.StatusOK {
			t.Errorf("request %d: code = %d, want 200", i, c)
		}
	}
	if got := atomic.LoadInt32(&bl.calls); got != 1 {
		t.Errorf("upstream calls = %d, want 1 (concurrent requests must dedupe)", got)
	}
}

func TestProviderModelsUnknownProvider(t *testing.T) {
	s := newModelsTestServer(t, providers.NewRegistry())
	code, _ := getModels(t, s, "ghost")
	if code != http.StatusNotFound {
		t.Errorf("code = %d, want 404", code)
	}
}

func TestProviderModelsUnsupportedKind(t *testing.T) {
	reg := providers.NewRegistry()
	reg.Register(&bareProvider{Provider: providers.NewMock("bare")}, 0)
	s := newModelsTestServer(t, reg)
	code, body := getModels(t, s, "bare")
	if code != http.StatusOK {
		t.Fatalf("code = %d, want 200", code)
	}
	if body["unsupported"] != true {
		t.Errorf("unsupported = %v, want true", body["unsupported"])
	}
}

func TestProviderModelsUpstreamErrorNotCached(t *testing.T) {
	cl := &countingLister{Provider: providers.NewMock("up"),
		err: &providers.Error{Status: 500, Message: "boom"}}
	reg := providers.NewRegistry()
	reg.Register(cl, 0)
	s := newModelsTestServer(t, reg)

	code, _ := getModels(t, s, "up")
	if code != http.StatusBadGateway {
		t.Fatalf("code = %d, want 502", code)
	}
	getModels(t, s, "up")
	if cl.calls != 2 {
		t.Errorf("upstream calls = %d, want 2 (errors are never cached)", cl.calls)
	}
}

// Silence unused import if llm is not needed after edits; llm.ChatRequest is
// referenced here to keep the import honest.
var _ = llm.ChatRequest{}
