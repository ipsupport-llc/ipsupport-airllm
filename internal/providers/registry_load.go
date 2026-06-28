package providers

import (
	"context"
	"log/slog"

	"github.com/rromenskyi/ipsupport-airllm/internal/secrets"
	"github.com/rromenskyi/ipsupport-airllm/internal/store"
)

// defaultBaseURL returns the public base URL for a provider kind; an explicit
// per-provider base_url overrides it.
func defaultBaseURL(kind string) string {
	switch kind {
	case "openai":
		return "https://api.openai.com/v1"
	case "openrouter":
		return "https://openrouter.ai/api/v1"
	case "xai":
		return "https://api.x.ai/v1"
	case "ollama":
		return "http://localhost:11434/v1"
	default:
		return ""
	}
}

// LoadFromStore builds a registry from the enabled providers, decrypting each
// stored API key. A mock provider is always available. Kinds without a client
// yet (anthropic-direct) are skipped with a warning.
func LoadFromStore(ctx context.Context, st *store.Store, sealer *secrets.Sealer) (*Registry, error) {
	rows, err := st.ListProvidersForRegistry(ctx)
	if err != nil {
		return nil, err
	}
	reg := NewRegistry()
	for _, p := range rows {
		apiKey := ""
		if len(p.CredEnc) > 0 {
			if pt, err := sealer.Open(p.CredEnc); err == nil {
				apiKey = string(pt)
			} else {
				slog.Error("provider credential decrypt failed; treating as unset", "provider", p.Name, "err", err)
			}
		}

		var prov Provider
		switch p.Kind {
		case "mock":
			prov = NewMock(p.Name)
		case "openai", "openrouter", "xai", "ollama":
			base := p.BaseURL
			if base == "" {
				base = defaultBaseURL(p.Kind)
			}
			prov = NewOpenAICompat(p.Name, p.Kind, base, apiKey)
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
