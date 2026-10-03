// Package ledger persists one usage row per gateway request. The ledger is
// the durable source of truth for reporting and reconciliation.
package ledger

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/store"
)

// Entry is a single usage record.
type Entry struct {
	KeyID            string // uuid; empty -> NULL
	UserID           string // uuid; empty -> NULL
	Alias            string
	ProviderName     string
	UpstreamModel    string
	IngressProtocol  string
	UpstreamProtocol string
	PromptTokens     int
	CompletionTokens int
	// ReasoningTokens is the share of CompletionTokens spent thinking — a
	// breakdown of that number, not an addition to it. See llm.Usage.
	ReasoningTokens int
	// Tier is the configured priority of the target that served the request
	// (or of the last one attempted, on failure). See routing.Target.Tier.
	Tier int
	// Attempts is how many upstream calls the request made across targets.
	Attempts  int
	CostUSD   float64
	Status    int
	LatencyMS int64
	ErrorMsg  string
}

// chanSize is the capacity of the internal work queue, sized the same way
// as internal/capture's: generous for a traffic burst, not for sustained
// overload — a usage row's insert is small and fast under normal load.
const chanSize = 1024

// workers is the fixed number of goroutines draining the queue, matching
// internal/capture's worker count for the same class of problem.
const workers = 4

// writeTimeout bounds each async insert so a stalled Postgres connection
// can't let queued work pile up indefinitely.
const writeTimeout = 5 * time.Second

// Ledger writes usage rows asynchronously: Record enqueues and returns
// immediately, never blocking the request path on a database round trip. A
// fixed worker pool drains a bounded queue; if the queue is full (Postgres
// falling behind under sustained load) the record is dropped and Dropped()
// increments, instead of adding latency or unbounded memory growth to the
// request path — metering is best-effort, as Record's doc already said.
type Ledger struct {
	st      *store.Store
	ch      chan Entry
	dropped atomic.Int64
	mu      sync.RWMutex // guards closed; held as RLock during channel send
	closed  bool
	wg      sync.WaitGroup
}

// New returns a Ledger backed by the store. Call Start to activate workers.
func New(st *store.Store) *Ledger {
	return &Ledger{st: st, ch: make(chan Entry, chanSize)}
}

// Start launches the worker pool that drains the queue. Mirrors
// internal/capture.Pipeline's New-then-Start shape: entries recorded before
// Start is called simply queue up (and drop past chanSize) until workers
// are running.
func (l *Ledger) Start() {
	for i := 0; i < workers; i++ {
		l.wg.Add(1)
		go func() {
			defer l.wg.Done()
			for e := range l.ch {
				l.write(e)
			}
		}()
	}
}

// Stop closes the queue, waits for every buffered entry to be written, and
// makes Record a safe no-op afterward. Call it once during graceful
// shutdown so an in-flight request's usage row isn't lost to a closing
// process — the ledger is the durable source of truth, unlike capture's
// sampled, already-lossy-by-design records.
//
// The write lock is held while closing the channel so no concurrent Record
// (which holds a read lock during the send) can race against close,
// mirroring internal/capture.Pipeline.Stop exactly.
func (l *Ledger) Stop() {
	l.mu.Lock()
	l.closed = true
	close(l.ch)
	l.mu.Unlock()
	l.wg.Wait()
}

// Dropped returns how many entries were dropped because the queue was full.
func (l *Ledger) Dropped() int64 { return l.dropped.Load() }

// Record enqueues one usage row for asynchronous persistence. It never
// blocks the caller on a database round trip: if the queue is full the
// record is dropped (Dropped() increments and a warning is logged) rather
// than adding latency to the request path. Safe to call after Stop (a
// no-op). ctx is not used for the write itself — the insert outlives the
// request by design, same as internal/webhook.Send and
// internal/capture.Pipeline.Enqueue.
func (l *Ledger) Record(_ context.Context, e Entry) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.closed {
		return
	}
	select {
	case l.ch <- e:
	default:
		l.dropped.Add(1)
		slog.Warn("ledger record dropped; queue full", "provider", e.ProviderName, "model", e.UpstreamModel)
	}
}

func (l *Ledger) write(e Entry) {
	ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
	defer cancel()
	_, err := l.st.PG.Exec(ctx, `
		INSERT INTO usage_ledger (
			key_id, user_id, alias, provider_name, upstream_model,
			ingress_protocol, upstream_protocol,
			prompt_tokens, completion_tokens, reasoning_tokens, cost_usd,
			status, latency_ms, error, tier, attempts
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`,
		nullUUID(e.KeyID), nullUUID(e.UserID), e.Alias, e.ProviderName, e.UpstreamModel,
		e.IngressProtocol, e.UpstreamProtocol,
		e.PromptTokens, e.CompletionTokens, e.ReasoningTokens, e.CostUSD,
		e.Status, e.LatencyMS, e.ErrorMsg, e.Tier, e.Attempts,
	)
	if err != nil {
		slog.Error("ledger record failed", "err", err, "provider", e.ProviderName, "model", e.UpstreamModel)
	}
}

func nullUUID(s string) any {
	if s == "" {
		return nil
	}
	return s
}
