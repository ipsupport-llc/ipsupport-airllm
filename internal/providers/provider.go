// Package providers defines the upstream provider interface, a concurrency-
// aware registry, and the provider implementations (mock + OpenAI-compatible).
package providers

import (
	"context"
	"sync/atomic"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/llm"
)

// Provider is one upstream LLM backend.
type Provider interface {
	// Name is the unique provider name (matches providers.name in the DB).
	Name() string
	// Kind is the provider family: openai | openrouter | xai | groq | ollama | anthropic | mock.
	Kind() string
	// Protocol is the native wire protocol: openai | anthropic.
	Protocol() string
	// Chat performs a non-streaming chat completion.
	Chat(ctx context.Context, req llm.ChatRequest) (llm.ChatResponse, error)
	// ChatStream performs a streaming chat completion, invoking yield for each
	// chunk in order. If yield returns an error (e.g. the client disconnected),
	// streaming stops and that error is returned.
	ChatStream(ctx context.Context, req llm.ChatRequest, yield func(llm.StreamChunk) error) error
}

// ModelLister is implemented by providers that can enumerate their upstream
// model ids. Providers without a listing endpoint simply do not implement it.
type ModelLister interface {
	ListModels(ctx context.Context) ([]string, error)
}

// ModelPrice is a catalog entry's price in USD per 1M tokens.
type ModelPrice struct {
	ID          string
	InputPer1M  float64
	OutputPer1M float64
}

// PricedModelLister is implemented by providers whose catalog publishes
// prices (OpenRouter). Entries without pricing are omitted.
type PricedModelLister interface {
	ListModelPricing(ctx context.Context) ([]ModelPrice, error)
}

// Entry wraps a provider with a concurrency limit. A request must Acquire a
// slot before calling and Release it after; when the limit is reached Acquire
// fails so the router can fall back to another target.
type Entry struct {
	Provider Provider
	sem      chan struct{} // nil = unlimited
	inflight atomic.Int64
	maxConc  int
}

// Acquire tries to take a concurrency slot without blocking.
func (e *Entry) Acquire() bool {
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

// Release returns a previously acquired slot.
func (e *Entry) Release() {
	e.inflight.Add(-1)
	if e.sem != nil {
		<-e.sem
	}
}

// Free reports how many more concurrent requests the provider can accept right
// now (a large value when unlimited), for least-busy balancing.
func (e *Entry) Free() int {
	if e.maxConc <= 0 {
		return 1 << 30
	}
	if f := e.maxConc - int(e.inflight.Load()); f > 0 {
		return f
	}
	return 0
}

// Registry holds entries by provider name. It is built once and treated as
// immutable; hot reloads swap in a freshly built Registry.
type Registry struct {
	byName map[string]*Entry
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{byName: make(map[string]*Entry)}
}

// Register adds a provider with a max concurrency (0 = unlimited).
func (r *Registry) Register(p Provider, maxConcurrency int) {
	e := &Entry{Provider: p, maxConc: maxConcurrency}
	if maxConcurrency > 0 {
		e.sem = make(chan struct{}, maxConcurrency)
	}
	r.byName[p.Name()] = e
}

// Get returns the entry for a provider name.
func (r *Registry) Get(name string) (*Entry, bool) {
	e, ok := r.byName[name]
	return e, ok
}
