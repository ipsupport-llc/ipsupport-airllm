package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/llm"
)

// The live view: which data-plane requests are running right now.
//
// Each pod keeps its in-flight requests in memory (liveRegistry) and
// publishes a snapshot of them to Redis every livePublishInterval under
// air:live:pod:<pod>, with a TTL a few intervals long, so a pod that dies
// drops out on its own. GET /api/admin/live reads every pod's key and
// merges them. Nothing here holds prompt or response content or a key
// secret: only who, where, which route and how far along.

// Request phases, as the API reports them.
const (
	phaseRouting   = "routing"   // received; no upstream attempt yet
	phaseUpstream  = "upstream"  // an upstream attempt is running
	phaseStreaming = "streaming" // the response is being streamed to the client
)

const (
	livePodKeyPrefix    = "air:live:pod:"
	livePublishInterval = 2 * time.Second
	livePodTTL          = 10 * time.Second
)

// liveRequest is one in-flight request on the wire, both in the Redis pod
// snapshot and in the API response. ElapsedMS is filled in when the API
// answers, against its own clock.
type liveRequest struct {
	ID            string    `json:"id"`
	Pod           string    `json:"pod"`
	StartedAt     time.Time `json:"started_at"`
	ElapsedMS     int64     `json:"elapsed_ms"`
	Endpoint      string    `json:"endpoint"`
	Stream        bool      `json:"stream"`
	KeyID         string    `json:"key_id"`
	KeyName       string    `json:"key_name"`
	User          string    `json:"user"`
	SessionID     string    `json:"session_id"`
	ClientIP      string    `json:"client_ip"`
	Alias         string    `json:"alias"`
	Provider      string    `json:"provider"`
	UpstreamModel string    `json:"upstream_model"`
	Attempt       int       `json:"attempt"`
	Phase         string    `json:"phase"`
	OutputTokens  int64     `json:"output_tokens"`
	BytesOut      int64     `json:"bytes_out"`
}

// liveRecord is the mutable state of one in-flight request. Every method is
// safe on a nil receiver, so code that runs outside a tracked request (tests,
// the second-pass job) calls them without checking.
type liveRecord struct {
	mu sync.Mutex
	r  liveRequest // ElapsedMS, OutputTokens and BytesOut unused here

	outputTokens atomic.Int64
	bytesOut     atomic.Int64
}

type liveCtxKey struct{}

func withLive(ctx context.Context, rec *liveRecord) context.Context {
	return context.WithValue(ctx, liveCtxKey{}, rec)
}

// liveFrom returns the request's live record, or nil when it is not tracked.
func liveFrom(ctx context.Context) *liveRecord {
	rec, _ := ctx.Value(liveCtxKey{}).(*liveRecord)
	return rec
}

// setAlias records the model name the client asked for.
func (l *liveRecord) setAlias(alias string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.r.Alias = alias
	l.mu.Unlock()
}

// setStream marks the request as a streaming one.
func (l *liveRecord) setStream() {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.r.Stream = true
	l.mu.Unlock()
}

// startAttempt records that upstream attempt n, to provider/model, began.
// The running token count restarts: a failed attempt's output never reached
// the client.
func (l *liveRecord) startAttempt(provider, upstreamModel string, n int) {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.r.Provider, l.r.UpstreamModel, l.r.Attempt, l.r.Phase = provider, upstreamModel, n, phaseUpstream
	l.mu.Unlock()
	l.outputTokens.Store(0)
}

// streaming records that the response started streaming to the client.
func (l *liveRecord) streaming() {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.r.Phase = phaseStreaming
	l.mu.Unlock()
}

// observeChunk keeps the best-effort output token count: one per chunk that
// carries output, until the upstream reports its real count.
func (l *liveRecord) observeChunk(c llm.StreamChunk) {
	if l == nil {
		return
	}
	switch {
	case c.Usage != nil && c.Usage.CompletionTokens > 0:
		l.outputTokens.Store(int64(c.Usage.CompletionTokens))
	case c.Content != "" || len(c.ToolCalls) > 0:
		l.outputTokens.Add(1)
	}
}

func (l *liveRecord) snapshot() liveRequest {
	l.mu.Lock()
	out := l.r
	l.mu.Unlock()
	out.OutputTokens = l.outputTokens.Load()
	out.BytesOut = l.bytesOut.Load()
	return out
}

// liveRegistry is this pod's set of in-flight requests.
type liveRegistry struct {
	pod string
	mu  sync.Mutex
	m   map[*liveRecord]struct{}
}

func newLiveRegistry(pod string) *liveRegistry {
	return &liveRegistry{pod: pod, m: map[*liveRecord]struct{}{}}
}

// register adds a request; r's ID, Pod, StartedAt and Phase are filled in.
func (g *liveRegistry) register(r liveRequest, now time.Time) *liveRecord {
	r.ID, r.Pod, r.StartedAt, r.Phase = newID(), g.pod, now.UTC(), phaseRouting
	rec := &liveRecord{r: r}
	g.mu.Lock()
	g.m[rec] = struct{}{}
	g.mu.Unlock()
	return rec
}

func (g *liveRegistry) deregister(rec *liveRecord) {
	g.mu.Lock()
	delete(g.m, rec)
	g.mu.Unlock()
}

// snapshot returns every in-flight request, oldest first.
func (g *liveRegistry) snapshot() []liveRequest {
	g.mu.Lock()
	recs := make([]*liveRecord, 0, len(g.m))
	for rec := range g.m {
		recs = append(recs, rec)
	}
	g.mu.Unlock()
	out := make([]liveRequest, 0, len(recs))
	for _, rec := range recs {
		out = append(out, rec.snapshot())
	}
	sortLive(out)
	return out
}

func sortLive(rs []liveRequest) {
	sort.Slice(rs, func(i, j int) bool {
		if !rs[i].StartedAt.Equal(rs[j].StartedAt) {
			return rs[i].StartedAt.Before(rs[j].StartedAt)
		}
		return rs[i].ID < rs[j].ID
	})
}

// podName names this replica: the pod's hostname under Kubernetes.
func podName() string {
	if h := os.Getenv("HOSTNAME"); h != "" {
		return h
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "unknown"
}

// trackLive registers a data-plane request in the live view for as long as
// next runs. It sits inside requireAPIKey, so only authenticated requests
// are listed.
func (s *Server) trackLive(endpoint string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.live == nil {
			next(w, r)
			return
		}
		ak, _ := keyFromContext(r.Context())
		rec := s.live.register(liveRequest{
			Endpoint:  endpoint,
			KeyID:     ak.KeyID,
			KeyName:   ak.KeyName,
			User:      ak.Owner,
			SessionID: clientSessionFrom(r.Context()),
			ClientIP:  s.clientIP(r),
		}, time.Now())
		defer s.live.deregister(rec)
		next(&liveWriter{ResponseWriter: w, rec: rec}, r.WithContext(withLive(r.Context(), rec)))
	}
}

// liveWriter counts the bytes written to the client.
type liveWriter struct {
	http.ResponseWriter
	rec *liveRecord
}

func (w *liveWriter) Write(b []byte) (int, error) {
	n, err := w.ResponseWriter.Write(b)
	w.rec.bytesOut.Add(int64(n))
	return n, err
}

// Flush forwards to the underlying writer, so streaming handlers that
// type-assert http.Flusher keep working. A handler that flushes is streaming.
func (w *liveWriter) Flush() {
	w.rec.setStream()
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *liveWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// livePod is one pod's entry in the API response.
type livePod struct {
	Pod      string    `json:"pod"`
	SeenAt   time.Time `json:"seen_at"`
	Inflight int       `json:"inflight"`
}

// liveResponse is the body of GET /api/admin/live.
type liveResponse struct {
	GeneratedAt time.Time     `json:"generated_at"`
	Pods        []livePod     `json:"pods"`
	Partial     bool          `json:"partial"`
	Requests    []liveRequest `json:"requests"`
}

// podSnapshot is what a pod publishes under its Redis key.
type podSnapshot struct {
	Pod      string        `json:"pod"`
	SeenAt   time.Time     `json:"seen_at"`
	Requests []liveRequest `json:"requests"`
}

func (s *Server) liveRDB() *redis.Client {
	if s.st == nil {
		return nil
	}
	return s.st.RDB
}

// handleAdminLive lists every in-flight request across all replicas. When
// Redis cannot be read it answers with this pod's own requests and
// partial: true.
func (s *Server) handleAdminLive(w http.ResponseWriter, r *http.Request) {
	now := time.Now().UTC()
	local := podSnapshot{SeenAt: now, Requests: []liveRequest{}}
	if s.live != nil {
		local.Pod, local.Requests = s.live.pod, s.live.snapshot()
	}
	pods, err := readLivePods(r.Context(), s.liveRDB(), now)
	partial := err != nil
	if partial {
		pods = nil
	}
	writeJSON(w, http.StatusOK, mergeLive(now, local, pods, partial))
}

// mergeLive builds the response from this pod's fresh snapshot and the ones
// read from Redis; this pod's own published one is replaced by the fresh one.
func mergeLive(now time.Time, local podSnapshot, published []podSnapshot, partial bool) liveResponse {
	all := make([]podSnapshot, 0, len(published)+1)
	if local.Pod != "" {
		all = append(all, local)
	}
	for _, p := range published {
		if p.Pod != local.Pod {
			all = append(all, p)
		}
	}
	out := liveResponse{GeneratedAt: now, Partial: partial, Pods: []livePod{}, Requests: []liveRequest{}}
	for _, p := range all {
		out.Pods = append(out.Pods, livePod{Pod: p.Pod, SeenAt: p.SeenAt, Inflight: len(p.Requests)})
		for _, rq := range p.Requests {
			rq.Pod = p.Pod
			rq.ElapsedMS = max(now.Sub(rq.StartedAt).Milliseconds(), 0)
			out.Requests = append(out.Requests, rq)
		}
	}
	sort.Slice(out.Pods, func(i, j int) bool { return out.Pods[i].Pod < out.Pods[j].Pod })
	sortLive(out.Requests)
	return out
}

// readLivePods reads every pod's published snapshot. A pod that only
// refreshed its key's TTL since its last write (it has been idle) is seen at
// the time of that refresh, worked out from what is left of the TTL.
func readLivePods(ctx context.Context, rdb *redis.Client, now time.Time) ([]podSnapshot, error) {
	if rdb == nil {
		return nil, errors.New("no redis")
	}
	var keys []string
	iter := rdb.Scan(ctx, 0, livePodKeyPrefix+"*", 100).Iterator()
	for iter.Next(ctx) {
		keys = append(keys, iter.Val())
	}
	if err := iter.Err(); err != nil {
		return nil, err
	}
	if len(keys) == 0 {
		return nil, nil
	}
	pipe := rdb.Pipeline()
	gets := make([]*redis.StringCmd, len(keys))
	ttls := make([]*redis.DurationCmd, len(keys))
	for i, k := range keys {
		gets[i] = pipe.Get(ctx, k)
		ttls[i] = pipe.PTTL(ctx, k)
	}
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return nil, err
	}
	out := make([]podSnapshot, 0, len(keys))
	for i := range keys {
		raw, err := gets[i].Bytes()
		if err != nil {
			continue // expired between SCAN and GET
		}
		var p podSnapshot
		if json.Unmarshal(raw, &p) != nil || p.Pod == "" {
			continue
		}
		if ttl, err := ttls[i].Result(); err == nil && ttl > 0 {
			if refreshed := now.Add(ttl - livePodTTL); refreshed.After(p.SeenAt) {
				p.SeenAt = refreshed
			}
		}
		out = append(out, p)
	}
	return out, nil
}

// StartLivePublisher publishes this pod's in-flight requests to Redis every
// livePublishInterval until the returned stop is called; stop deletes the
// pod's key. It runs independently of the serving context so the requests
// that are still finishing during a graceful shutdown stay visible.
func (s *Server) StartLivePublisher() (stop func()) {
	rdb := s.liveRDB()
	if s.live == nil || rdb == nil {
		return func() {}
	}
	key := livePodKeyPrefix + s.live.pod
	done, exited := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(exited)
		t := time.NewTicker(livePublishInterval)
		defer t.Stop()
		p := livePublisher{rdb: rdb, key: key, reg: s.live}
		for {
			p.publish()
			select {
			case <-done:
				return
			case <-t.C:
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			close(done)
			<-exited
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = rdb.Del(ctx, key).Err()
		})
	}
}

// livePublisher writes one pod's snapshot. An idle pod that was already idle
// at the last write only refreshes its key's TTL, which keeps it listed.
type livePublisher struct {
	rdb       *redis.Client
	key       string
	reg       *liveRegistry
	wasIdle   bool // the last write was an empty snapshot
	published bool // there has been a successful write
	failing   bool
}

func (p *livePublisher) publish() {
	ctx, cancel := context.WithTimeout(context.Background(), livePublishInterval)
	defer cancel()
	reqs := p.reg.snapshot()
	idle := len(reqs) == 0
	err := func() error {
		if idle && p.wasIdle && p.published {
			refreshed, err := p.rdb.PExpire(ctx, p.key, livePodTTL).Result()
			if err != nil || refreshed {
				return err
			}
			// The key is gone (Redis restarted, or it expired): write it again.
		}
		raw, err := json.Marshal(podSnapshot{Pod: p.reg.pod, SeenAt: time.Now().UTC(), Requests: reqs})
		if err != nil {
			return err
		}
		return p.rdb.Set(ctx, p.key, raw, livePodTTL).Err()
	}()
	if err != nil {
		if !p.failing {
			slog.Warn("live view: publishing this pod's in-flight requests failed", "err", err)
		}
		p.failing = true
		p.published = false
		return
	}
	if p.failing {
		slog.Info("live view: publishing recovered")
	}
	p.failing, p.published, p.wasIdle = false, true, idle
}
