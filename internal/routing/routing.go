// Package routing resolves a client-requested model into an ordered list of
// upstream targets. It expands the alias catalog (with fallback order) and
// supports explicit "provider/model" routing for keys permitted to use it.
package routing

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/rromenskyi/ipsupport-airouter/internal/store"
)

// Target is one resolved upstream destination.
type Target struct {
	Provider         string
	UpstreamModel    string
	UpstreamProtocol string
}

// Router resolves models against the catalog in the store.
type Router struct {
	st *store.Store
}

// NewRouter returns a Router backed by the store.
func NewRouter(st *store.Store) *Router { return &Router{st: st} }

// Resolve returns the ordered targets for a requested model. A model of the
// form "provider/upstream-model" routes directly when allowPassthrough is
// set; otherwise the alias catalog is expanded by ascending priority.
func (r *Router) Resolve(ctx context.Context, model string, allowPassthrough bool) ([]Target, error) {
	if strings.Contains(model, "/") {
		if !allowPassthrough {
			return nil, fmt.Errorf("explicit provider routing is not permitted for this key")
		}
		provider, upstream, _ := strings.Cut(model, "/")
		t, err := r.passthroughTarget(ctx, provider, upstream)
		if err != nil {
			return nil, err
		}
		return []Target{t}, nil
	}

	rows, err := r.st.PG.Query(ctx, `
		SELECT t.provider_name, t.upstream_model, t.upstream_protocol
		FROM alias_targets t
		JOIN providers p ON p.name = t.provider_name AND p.enabled = true
		WHERE t.alias = $1
		ORDER BY t.priority`, model)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var targets []Target
	for rows.Next() {
		var t Target
		if err := rows.Scan(&t.Provider, &t.UpstreamModel, &t.UpstreamProtocol); err != nil {
			return nil, err
		}
		targets = append(targets, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("model %q not found", model)
	}
	return targets, nil
}

func (r *Router) passthroughTarget(ctx context.Context, provider, upstreamModel string) (Target, error) {
	if upstreamModel == "" {
		return Target{}, fmt.Errorf("explicit model %q missing upstream name", provider+"/")
	}
	var kind string
	err := r.st.PG.QueryRow(ctx,
		`SELECT kind FROM providers WHERE name = $1 AND enabled = true`, provider,
	).Scan(&kind)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Target{}, fmt.Errorf("provider %q not found", provider)
		}
		return Target{}, err
	}
	proto := "openai"
	if kind == "anthropic" {
		proto = "anthropic"
	}
	return Target{Provider: provider, UpstreamModel: upstreamModel, UpstreamProtocol: proto}, nil
}
