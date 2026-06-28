// Package providers defines the upstream provider interface and a registry.
// v1 ships a mock provider; real providers (OpenAI, OpenRouter, xAI,
// Anthropic) implement the same interface and are added one file each.
package providers

import (
	"context"

	"github.com/rromenskyi/ipsupport-airouter/internal/llm"
)

// Provider is one upstream LLM backend.
type Provider interface {
	// Name is the unique provider name (matches providers.name in the DB).
	Name() string
	// Kind is the provider family: openai | openrouter | xai | anthropic | mock.
	Kind() string
	// Protocol is the native wire protocol: openai | anthropic. Used to
	// decide passthrough vs translation.
	Protocol() string
	// Chat performs a non-streaming chat completion.
	Chat(ctx context.Context, req llm.ChatRequest) (llm.ChatResponse, error)
	// ChatStream performs a streaming chat completion, invoking yield for
	// each chunk in order. If yield returns an error (e.g. the client
	// disconnected), streaming stops and that error is returned.
	ChatStream(ctx context.Context, req llm.ChatRequest, yield func(llm.StreamChunk) error) error
}

// Registry holds providers by name.
type Registry struct {
	byName map[string]Provider
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{byName: make(map[string]Provider)}
}

// Register adds (or replaces) a provider by its name.
func (r *Registry) Register(p Provider) {
	r.byName[p.Name()] = p
}

// Get returns a provider by name.
func (r *Registry) Get(name string) (Provider, bool) {
	p, ok := r.byName[name]
	return p, ok
}
