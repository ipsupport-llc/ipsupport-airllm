package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/llm"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/providers"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/routing"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/store"
)

func TestLiveRegistryTracksARequestThroughItsLife(t *testing.T) {
	g := newLiveRegistry("pod-a")
	start := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	rec := g.register(liveRequest{Endpoint: "chat", KeyID: "k1", KeyName: "laptop", User: "alice", SessionID: "s1", ClientIP: "10.0.0.5"}, start)

	got := g.snapshot()
	if len(got) != 1 {
		t.Fatalf("snapshot has %d requests, want 1", len(got))
	}
	r := got[0]
	if r.ID == "" || r.Pod != "pod-a" || !r.StartedAt.Equal(start) || r.Phase != phaseRouting || r.Attempt != 0 {
		t.Fatalf("fresh request = %+v, want an id, pod-a, its start and the routing phase", r)
	}
	if r.Endpoint != "chat" || r.KeyName != "laptop" || r.User != "alice" || r.SessionID != "s1" || r.ClientIP != "10.0.0.5" {
		t.Fatalf("request lost its identity: %+v", r)
	}

	rec.setAlias("sonnet")
	rec.setStream()
	rec.startAttempt("p1", "m1", 1)
	rec.observeChunk(llm.StreamChunk{Content: "a"})
	rec.startAttempt("p2", "m2", 2) // failover: the count starts over
	rec.observeChunk(llm.StreamChunk{Content: "b"})
	rec.observeChunk(llm.StreamChunk{})
	rec.streaming()
	rec.observeChunk(llm.StreamChunk{Content: "c"})
	rec.bytesOut.Add(42)

	r = g.snapshot()[0]
	want := liveRequest{ID: r.ID, Pod: "pod-a", StartedAt: start, Endpoint: "chat", Stream: true,
		KeyID: "k1", KeyName: "laptop", User: "alice", SessionID: "s1", ClientIP: "10.0.0.5",
		Alias: "sonnet", Provider: "p2", UpstreamModel: "m2", Attempt: 2, Phase: phaseStreaming,
		OutputTokens: 2, BytesOut: 42}
	if r != want {
		t.Fatalf("updated request =\n%+v\nwant\n%+v", r, want)
	}

	rec.observeChunk(llm.StreamChunk{Usage: &llm.Usage{CompletionTokens: 17}})
	if r := g.snapshot()[0]; r.OutputTokens != 17 {
		t.Errorf("output_tokens = %d after the upstream reported 17", r.OutputTokens)
	}

	g.deregister(rec)
	if got := g.snapshot(); len(got) != 0 {
		t.Fatalf("deregistered request still listed: %+v", got)
	}
}

func TestLiveRecordMethodsAreNilSafe(t *testing.T) {
	rec := liveFrom(context.Background())
	if rec != nil {
		t.Fatal("untracked context returned a record")
	}
	rec.setAlias("a")
	rec.setStream()
	rec.startAttempt("p", "m", 1)
	rec.streaming()
	rec.observeChunk(llm.StreamChunk{Content: "x"})
}

func TestLiveRegistryIsSafeUnderConcurrentUse(t *testing.T) {
	g := newLiveRegistry("pod-a")
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() { // a publisher and an admin reading all the while
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = g.snapshot()
			}
		}
	}()
	var workers sync.WaitGroup
	for i := 0; i < 16; i++ {
		workers.Add(1)
		go func(i int) {
			defer workers.Done()
			for j := 0; j < 200; j++ {
				rec := g.register(liveRequest{Endpoint: "chat"}, time.Now())
				rec.setAlias(fmt.Sprintf("a%d", i))
				rec.startAttempt("p", "m", j)
				rec.observeChunk(llm.StreamChunk{Content: "x"})
				rec.streaming()
				rec.bytesOut.Add(1)
				g.deregister(rec)
			}
		}(i)
	}
	workers.Wait()
	close(stop)
	wg.Wait()
	if got := g.snapshot(); len(got) != 0 {
		t.Fatalf("%d requests left after every one was deregistered", len(got))
	}
}

func TestMergeLiveCombinesPodsAndPrefersTheLocalSnapshot(t *testing.T) {
	now := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	at := func(sec int) time.Time { return now.Add(time.Duration(sec) * time.Second) }
	local := podSnapshot{Pod: "pod-a", SeenAt: now, Requests: []liveRequest{{ID: "a1", StartedAt: at(-5)}}}
	published := []podSnapshot{
		// pod-a's own stale publication: replaced by the fresh local one.
		{Pod: "pod-a", SeenAt: at(-2), Requests: []liveRequest{{ID: "old", StartedAt: at(-30)}}},
		{Pod: "pod-b", SeenAt: at(-1), Requests: []liveRequest{{ID: "b1", StartedAt: at(-20)}, {ID: "b2", StartedAt: at(-1)}}},
		{Pod: "pod-c", SeenAt: at(-3), Requests: []liveRequest{}},
	}
	out := mergeLive(now, local, published, false)

	if out.Partial || !out.GeneratedAt.Equal(now) {
		t.Errorf("partial=%v generated_at=%v", out.Partial, out.GeneratedAt)
	}
	pods := fmt.Sprint(out.Pods)
	wantPods := fmt.Sprint([]livePod{{"pod-a", now, 1}, {"pod-b", at(-1), 2}, {"pod-c", at(-3), 0}})
	if pods != wantPods {
		t.Errorf("pods = %s, want %s", pods, wantPods)
	}
	var ids []string
	for _, r := range out.Requests {
		ids = append(ids, fmt.Sprintf("%s@%s/%d", r.ID, r.Pod, r.ElapsedMS))
	}
	if got, want := strings.Join(ids, " "), "b1@pod-b/20000 a1@pod-a/5000 b2@pod-b/1000"; got != want {
		t.Errorf("requests = %s, want %s (oldest first, elapsed from generated_at)", got, want)
	}
}

func TestAdminLiveIsPartialWithoutRedis(t *testing.T) {
	// Nothing listens on this address: every Redis call fails at once.
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 50 * time.Millisecond, MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	s := &Server{st: &store.Store{RDB: rdb}, live: newLiveRegistry("pod-a")}
	rec := s.live.register(liveRequest{Endpoint: "chat"}, time.Now())
	defer s.live.deregister(rec)

	out := getLive(t, s)
	if !out.Partial {
		t.Error("partial = false with Redis down")
	}
	if len(out.Pods) != 1 || out.Pods[0].Pod != "pod-a" || len(out.Requests) != 1 || out.Requests[0].ID != rec.snapshot().ID {
		t.Errorf("want this pod's own request; got pods=%+v requests=%+v", out.Pods, out.Requests)
	}
}

func TestAdminLiveMergesReplicasThroughRedis(t *testing.T) {
	rdb := testBreakerRedis(t)
	suffix := fmt.Sprint(time.Now().UnixNano())
	a := &Server{st: &store.Store{RDB: rdb}, live: newLiveRegistry("live-a-" + suffix)}
	b := &Server{st: &store.Store{RDB: rdb}, live: newLiveRegistry("live-b-" + suffix)}
	ra := a.live.register(liveRequest{Endpoint: "chat", KeyName: "ka"}, time.Now().Add(-time.Second))
	defer a.live.deregister(ra)
	rb := b.live.register(liveRequest{Endpoint: "messages", KeyName: "kb"}, time.Now().Add(-2*time.Second))
	rb.startAttempt("prov", "model", 1)

	stopB := b.StartLivePublisher()
	defer stopB()
	waitFor(t, func() bool { return rdb.Exists(context.Background(), livePodKeyPrefix+b.live.pod).Val() == 1 })

	out := getLive(t, a)
	if out.Partial {
		t.Error("partial = true with Redis up")
	}
	got := map[string]liveRequest{}
	for _, r := range out.Requests {
		got[r.Pod] = r
	}
	if r := got[a.live.pod]; r.KeyName != "ka" {
		t.Errorf("replica a's own request missing: %+v", out.Requests)
	}
	if r := got[b.live.pod]; r.KeyName != "kb" || r.Provider != "prov" || r.Phase != phaseUpstream || r.ElapsedMS < 2000 {
		t.Errorf("replica b's request = %+v, want kb on prov upstream for at least 2s", r)
	}
	if !hasPod(out.Pods, a.live.pod, 1) || !hasPod(out.Pods, b.live.pod, 1) {
		t.Errorf("pods = %+v, want both replicas with one request each", out.Pods)
	}

	// Once b's request ends, its next publication empties it; b stays listed.
	b.live.deregister(rb)
	waitFor(t, func() bool { return hasPod(getLive(t, a).Pods, b.live.pod, 0) })

	// Idle after idle only refreshes the key's TTL: its payload is not rewritten.
	key := livePodKeyPrefix + b.live.pod
	before := rdb.Get(context.Background(), key).Val()
	time.Sleep(livePublishInterval + 500*time.Millisecond)
	if after := rdb.Get(context.Background(), key).Val(); after != before {
		t.Errorf("idle pod rewrote its snapshot:\n%s\n%s", before, after)
	}
	if ttl := rdb.PTTL(context.Background(), key).Val(); ttl < livePodTTL-livePublishInterval-time.Second {
		t.Errorf("idle pod's key TTL = %v, want it refreshed", ttl)
	}
	if p := findPod(getLive(t, a).Pods, b.live.pod); p == nil || time.Since(p.SeenAt) > livePublishInterval+time.Second {
		t.Errorf("idle pod seen_at = %+v, want the last heartbeat", p)
	}

	stopB()
	if n := rdb.Exists(context.Background(), key).Val(); n != 0 {
		t.Error("stopped publisher left its pod key behind")
	}
}

// blockingProvider streams one chunk, then holds the stream open until
// release is closed.
type blockingProvider struct {
	name    string
	started chan struct{}
	release chan struct{}
}

func (p *blockingProvider) Name() string     { return p.name }
func (p *blockingProvider) Kind() string     { return "openai" }
func (p *blockingProvider) Protocol() string { return "openai" }
func (p *blockingProvider) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, fmt.Errorf("unary not supported")
}
func (p *blockingProvider) ChatStream(ctx context.Context, _ llm.ChatRequest, yield func(llm.StreamChunk) error) error {
	if err := yield(llm.StreamChunk{Content: "hello"}); err != nil {
		return err
	}
	close(p.started)
	select {
	case <-p.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return yield(llm.StreamChunk{FinishReason: "stop", Usage: &llm.Usage{PromptTokens: 3, CompletionTokens: 5}})
}

func newBlockingProvider(name string) *blockingProvider {
	return &blockingProvider{name: name, started: make(chan struct{}), release: make(chan struct{})}
}

// TestLiveShowsAStreamWhileItRuns needs no database: it drives the shared
// execution path (runStream → executePlan) under trackLive.
func TestLiveShowsAStreamWhileItRuns(t *testing.T) {
	up := newBlockingProvider("blocker")
	s := newRunChatTestServer(t, up)
	s.live = newLiveRegistry("pod-a")
	plan := &routing.Plan{Alias: "sonnet", Strategy: "round_robin", Tiers: [][]routing.Target{{{Provider: "blocker", UpstreamModel: "up-model"}}}}
	h := s.trackLive("chat", func(w http.ResponseWriter, r *http.Request) {
		sink := &openaiSink{w: w, flush: w.(http.Flusher).Flush}
		_, _, _, _ = s.runStream(r.Context(), plan, llm.ChatRequest{Model: "sonnet", Stream: true}, sink)
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set("X-Forwarded-For", "203.0.113.9, 10.0.0.1")
	r.Header.Set("X-Session-Id", "call-1")
	r = r.WithContext(context.WithValue(withClientSession(r.Context(), r), keyCtxKey, authedKey{KeyID: "k1", KeyName: "laptop", Owner: "alice"}))

	done := make(chan struct{})
	go func() { defer close(done); h(httptest.NewRecorder(), r) }()
	<-up.started

	out := getLive(t, s)
	if len(out.Requests) != 1 {
		t.Fatalf("in-flight requests = %+v, want the running stream", out.Requests)
	}
	got := out.Requests[0]
	if got.Endpoint != "chat" || !got.Stream || got.Alias != "sonnet" || got.Provider != "blocker" || got.UpstreamModel != "up-model" ||
		got.Attempt != 1 || got.Phase != phaseStreaming || got.OutputTokens != 1 || got.BytesOut == 0 ||
		got.KeyID != "k1" || got.KeyName != "laptop" || got.User != "alice" || got.SessionID != "call-1" || got.ClientIP != "203.0.113.9" {
		t.Errorf("in-flight request = %+v", got)
	}

	close(up.release)
	<-done
	if out := getLive(t, s); len(out.Requests) != 0 {
		t.Errorf("finished request still listed: %+v", out.Requests)
	}
}

// TestLiveListsARealChatRequest goes through the real chat handler, so it
// needs the alias in Postgres.
func TestLiveListsARealChatRequest(t *testing.T) {
	name := fmt.Sprintf("live-blocker-%d", time.Now().UnixNano())
	up := newBlockingProvider(name)
	s := newAudioRig(t, up)
	s.live = newLiveRegistry("pod-a")
	alias := "live-" + name
	seedAudioAlias(t, s, alias, map[string]string{name: "openai"}, `[{"provider":"`+name+`","upstream_model":"m","priority":0}]`)

	r := asKey(httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"`+alias+`","stream":true,"messages":[{"role":"user","content":"hi"}]}`)), alias)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { defer close(done); s.trackLive("chat", s.handleChatCompletions)(rec, r) }()
	select {
	case <-up.started:
	case <-done:
		t.Fatalf("request ended before the upstream was called: %d %s", rec.Code, rec.Body.String())
	}

	out := getLive(t, s.Server)
	if len(out.Requests) != 1 || out.Requests[0].Alias != alias || out.Requests[0].Provider != name || !out.Requests[0].Stream {
		t.Fatalf("in-flight requests = %+v, want the running chat stream", out.Requests)
	}
	close(up.release)
	<-done
	if rec.Code != http.StatusOK {
		t.Fatalf("chat = %d %s", rec.Code, rec.Body.String())
	}
	if out := getLive(t, s.Server); len(out.Requests) != 0 {
		t.Errorf("finished request still listed: %+v", out.Requests)
	}
}

func getLive(t *testing.T, s *Server) liveResponse {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handleAdminLive(rec, httptest.NewRequest(http.MethodGet, "/api/admin/live", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/admin/live = %d %s", rec.Code, rec.Body.String())
	}
	var out liveResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

func findPod(pods []livePod, name string) *livePod {
	for i := range pods {
		if pods[i].Pod == name {
			return &pods[i]
		}
	}
	return nil
}

func hasPod(pods []livePod, name string, inflight int) bool {
	p := findPod(pods, name)
	return p != nil && p.Inflight == inflight
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * livePublishInterval)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

var _ providers.Provider = (*blockingProvider)(nil)
