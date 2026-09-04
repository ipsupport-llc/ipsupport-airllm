package providers

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/secrets"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/store"
)

// defaultBaseURL returns the public base URL for a provider kind; an explicit
// per-provider base_url overrides it. Vertex is absent on purpose: its address
// is assembled from the provider's configuration, see vertexBaseURL.
func defaultBaseURL(kind string) string {
	switch kind {
	case "openai":
		return "https://api.openai.com/v1"
	case "openrouter":
		return "https://openrouter.ai/api/v1"
	case "xai":
		return "https://api.x.ai/v1"
	case "groq":
		return "https://api.groq.com/openai/v1"
	case "ollama":
		return "http://localhost:11434/v1"
	default:
		return ""
	}
}

// LoadFromStore builds a registry from the enabled providers, decrypting each
// stored credential. A mock provider is always available. Kinds without a
// client yet (anthropic-direct) are skipped with a warning.
//
// A provider that cannot be built is skipped, never fatal: the gateway has to
// start — and keep serving every other provider — when one provider's
// credentials or configuration are wrong.
func LoadFromStore(ctx context.Context, st *store.Store, sealer *secrets.Sealer) (*Registry, error) {
	rows, err := st.ListProvidersForRegistry(ctx)
	if err != nil {
		return nil, err
	}
	reg := NewRegistry()
	for _, p := range rows {
		var cred []byte
		var credErr error
		if len(p.CredEnc) > 0 {
			if pt, err := sealer.Open(p.CredEnc); err == nil {
				cred = pt
			} else {
				slog.Error("provider credential decrypt failed; treating as unset", "provider", p.Name, "err", err)
				credErr = err
			}
		}

		var prov Provider
		switch p.Kind {
		case "mock":
			prov = NewMock(p.Name)
		case "openai", "openrouter", "xai", "groq", "ollama":
			base := p.BaseURL
			if base == "" {
				base = defaultBaseURL(p.Kind)
			}
			prov = NewOpenAICompat(p.Name, p.Kind, base, string(cred))
		case "vertex":
			v, err := newVertexFromRow(ctx, p, cred, credErr)
			if err != nil {
				slog.Error("vertex provider disabled", "provider", p.Name, "err", err)
				continue
			}
			prov = v
		default:
			slog.Warn("provider kind has no client yet; skipping", "provider", p.Name, "kind", p.Kind)
			continue
		}
		reg.Register(prov, p.MaxConcurrency)
	}

	if _, ok := reg.Get("mock"); !ok {
		reg.Register(NewMock("mock"), 0)
	}
	return reg, nil
}

// newVertexFromRow builds a Vertex provider from its stored row.
//
// A credential that was stored but could not be decrypted disables the
// provider rather than falling through to the ambient identity. That
// distinction only exists for this kind: for a static-key kind an unusable
// key produces an unauthenticated call that simply fails, but here it would
// silently run the gateway as the pod's own principal — a different identity
// than the operator configured, spending against a different grant.
func newVertexFromRow(ctx context.Context, p store.ProviderRow, cred []byte, credErr error) (*Vertex, error) {
	if credErr != nil {
		return nil, fmt.Errorf("stored credential could not be decrypted: %w", credErr)
	}
	cfg, err := parseVertexConfig(p.Config)
	if err != nil {
		return nil, err
	}
	if err := validateVertexConfig(cfg, p.BaseURL); err != nil {
		return nil, err
	}
	tokens, err := GoogleTokenSource(ctx, cred)
	if err != nil {
		return nil, err
	}
	return NewVertex(p.Name, vertexBaseURL(cfg, p.BaseURL), tokens), nil
}
