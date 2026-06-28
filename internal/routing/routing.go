// Package routing resolves a client-requested model into a fallback/load-
// balancing plan: targets grouped into priority tiers, with a within-tier
// strategy (round-robin or least-busy). Lower-priority tiers are fallback.
package routing

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/jackc/pgx/v5"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/store"
)

// Target is one resolved upstream destination.
type Target struct {
	Provider         string
	UpstreamModel    string
	UpstreamProtocol string
}

// Plan is the ordered set of priority tiers for a request, plus the within-
// tier balancing strategy.
type Plan struct {
	Alias    string
	Strategy string     // round_robin | least_busy
	Tiers    [][]Target // index 0 = highest priority (tried first)
}

// Ordered flattens the tiers into the try-order for one request: tier by tier,
// each tier internally ordered by the strategy. rr rotates round-robin;
// free(provider) drives least-busy.
func (p *Plan) Ordered(rr uint64, free func(provider string) int) []Target {
	var out []Target
	for _, tier := range p.Tiers {
		out = append(out, orderTier(tier, p.Strategy, rr, free)...)
	}
	return out
}

func orderTier(tier []Target, strategy string, rr uint64, free func(string) int) []Target {
	n := len(tier)
	if n <= 1 {
		return tier
	}
	if strategy == "least_busy" {
		idx := make([]int, n)
		for i := range idx {
			idx[i] = i
		}
		sort.SliceStable(idx, func(a, b int) bool {
			return free(tier[idx[a]].Provider) > free(tier[idx[b]].Provider)
		})
		out := make([]Target, n)
		for i, j := range idx {
			out[i] = tier[j]
		}
		return out
	}
	// round_robin: rotate the tier by rr.
	rot := int(rr % uint64(n))
	out := make([]Target, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, tier[(rot+i)%n])
	}
	return out
}

// Router resolves models against the catalog and keeps round-robin counters.
type Router struct {
	st *store.Store
	rr sync.Map // alias -> *atomic.Uint64
}

// NewRouter returns a Router backed by the store.
func NewRouter(st *store.Store) *Router { return &Router{st: st} }

// NextRR returns the next round-robin tick for an alias.
func (r *Router) NextRR(alias string) uint64 {
	v, _ := r.rr.LoadOrStore(alias, new(atomic.Uint64))
	return v.(*atomic.Uint64).Add(1) - 1
}

// Resolve builds the plan for a requested model. "provider/upstream-model"
// routes directly (single tier) when allowPassthrough is set; otherwise the
// alias catalog is expanded into priority tiers.
func (r *Router) Resolve(ctx context.Context, model string, allowPassthrough bool) (*Plan, error) {
	if strings.Contains(model, "/") {
		if !allowPassthrough {
			return nil, fmt.Errorf("explicit provider routing is not permitted for this key")
		}
		provider, upstream, _ := strings.Cut(model, "/")
		t, err := r.passthroughTarget(ctx, provider, upstream)
		if err != nil {
			return nil, err
		}
		return &Plan{Alias: model, Strategy: "round_robin", Tiers: [][]Target{{t}}}, nil
	}

	var strategy string
	err := r.st.PG.QueryRow(ctx, `SELECT strategy FROM model_aliases WHERE alias = $1`, model).Scan(&strategy)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("model %q not found", model)
		}
		return nil, err
	}

	rows, err := r.st.PG.Query(ctx, `
		SELECT t.priority, t.provider_name, t.upstream_model, t.upstream_protocol
		FROM alias_targets t
		JOIN providers p ON p.name = t.provider_name AND p.enabled = true
		WHERE t.alias = $1
		ORDER BY t.priority`, model)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tiers [][]Target
	lastPriority := -1
	for rows.Next() {
		var priority int
		var t Target
		if err := rows.Scan(&priority, &t.Provider, &t.UpstreamModel, &t.UpstreamProtocol); err != nil {
			return nil, err
		}
		if len(tiers) == 0 || priority != lastPriority {
			tiers = append(tiers, []Target{})
			lastPriority = priority
		}
		tiers[len(tiers)-1] = append(tiers[len(tiers)-1], t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(tiers) == 0 {
		return nil, fmt.Errorf("model %q has no available targets", model)
	}
	return &Plan{Alias: model, Strategy: strategy, Tiers: tiers}, nil
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
