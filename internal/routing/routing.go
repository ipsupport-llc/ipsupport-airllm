// Package routing resolves a client-requested model into a fallback/load-
// balancing plan: targets grouped into priority tiers, with a within-tier
// strategy (round-robin or least-busy). Lower-priority tiers are fallback.
package routing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/jackc/pgx/v5"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/lookupcache"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/store"
)

// Target is one resolved upstream destination.
type Target struct {
	Provider         string
	UpstreamModel    string
	UpstreamProtocol string
	// DisplayLabel is an operator-chosen name for this target, shown in the
	// X-Backend-Model response header (see Plan.ExposeBackendHeaders)
	// instead of Provider/UpstreamModel — those stay internal. Empty means
	// no label was configured for this target.
	DisplayLabel string
	// Options is the target's own failover policy (alias_targets.options).
	Options TargetOptions
	// Tier identifies the priority tier this target belongs to: the
	// configured alias_targets.priority, not its position in Plan.Tiers, so
	// it stays the same when a provider in another tier is disabled or
	// added. Lower is tried first; a passthrough target is tier 0.
	Tier int
}

// TargetOptions are the per-target knobs stored in alias_targets.options.
// The stored object is free-form so later behaviours can add keys without a
// migration; only the keys below are read here, and an unknown key is kept
// and ignored. A nil field means "not set on this target": the gateway-wide
// default from settings applies.
type TargetOptions struct {
	// TimeoutMS is the target's time budget. For a streamed chat it bounds
	// the wait for the first chunk; for a unary chat or an audio request it
	// bounds the whole call. 0 means no budget.
	TimeoutMS *int `json:"timeout_ms,omitempty"`
	// FallbackOnAuth makes an upstream authorisation or billing failure a
	// reason to try the next target instead of failing the request.
	FallbackOnAuth *bool `json:"fallback_on_auth,omitempty"`
	// Breaker overrides the circuit breaker thresholds for this target's
	// tier. The breaker is per tier, so when the targets of one tier
	// disagree, each key is taken from the first target that sets it.
	Breaker *BreakerOptions `json:"breaker,omitempty"`
}

// BreakerOptions are circuit breaker knobs, as stored in a target's options
// and in the gateway-wide failover defaults. A nil field is unset.
type BreakerOptions struct {
	// Enabled switches the breaker on for the tier.
	Enabled *bool `json:"enabled,omitempty"`
	// Failures is the run of consecutive failures that opens the tier.
	Failures *int `json:"failures,omitempty"`
	// ErrorRate opens the tier when more than this fraction (0..1] of the
	// requests in a window of WindowMS failed, once MinRequests were seen.
	ErrorRate   *float64 `json:"error_rate,omitempty"`
	WindowMS    *int     `json:"window_ms,omitempty"`
	MinRequests *int     `json:"min_requests,omitempty"`
	// CooldownMS is the first trip's cooldown; each re-trip doubles it up to
	// MaxCooldownMS, and it resets after StableMS without a trip.
	CooldownMS    *int `json:"cooldown_ms,omitempty"`
	MaxCooldownMS *int `json:"max_cooldown_ms,omitempty"`
	StableMS      *int `json:"stable_ms,omitempty"`
}

// Validate rejects out-of-range values: every count and duration must be
// positive, and the error rate must lie in (0, 1].
func (b *BreakerOptions) Validate() error {
	if b == nil {
		return nil
	}
	for _, f := range []struct {
		name string
		v    *int
	}{
		{"failures", b.Failures}, {"window_ms", b.WindowMS}, {"min_requests", b.MinRequests},
		{"cooldown_ms", b.CooldownMS}, {"max_cooldown_ms", b.MaxCooldownMS}, {"stable_ms", b.StableMS},
	} {
		if f.v != nil && *f.v <= 0 {
			return fmt.Errorf("breaker.%s must be positive", f.name)
		}
	}
	if b.ErrorRate != nil && (*b.ErrorRate <= 0 || *b.ErrorRate > 1) {
		return fmt.Errorf("breaker.error_rate must be in (0, 1]")
	}
	return nil
}

// ParseTargetOptions decodes a stored or submitted options object. Empty
// input and JSON null are the empty object. Anything that is not an object,
// or a known key of the wrong type or out of range, is an error.
func ParseTargetOptions(raw []byte) (TargetOptions, error) {
	var o TargetOptions
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return o, nil
	}
	if !strings.HasPrefix(trimmed, "{") {
		return o, fmt.Errorf("options must be a JSON object")
	}
	if err := json.Unmarshal(raw, &o); err != nil {
		return o, fmt.Errorf("invalid options: %w", err)
	}
	if o.TimeoutMS != nil && *o.TimeoutMS < 0 {
		return o, fmt.Errorf("invalid options: timeout_ms must not be negative")
	}
	if err := o.Breaker.Validate(); err != nil {
		return o, fmt.Errorf("invalid options: %w", err)
	}
	return o, nil
}

// ErrModelNotFound is what Resolve's error wraps when the requested model
// is not an alias.
var ErrModelNotFound = errors.New("model not found")

type modelNotFoundError struct{ model string }

func (e modelNotFoundError) Error() string { return fmt.Sprintf("model %q not found", e.model) }
func (e modelNotFoundError) Unwrap() error { return ErrModelNotFound }

// Plan is the ordered set of priority tiers for a request, plus the within-
// tier balancing strategy.
type Plan struct {
	Alias                string
	Strategy             string     // round_robin | least_busy
	DLPModelScan         bool       // run the layer-2 BERT scan for this alias
	ExposeBackendHeaders bool       // set X-Backend-Provider/-Model on the response
	DLPAudioScan         bool       // run layer-1 DLP scanning on audio text for this alias
	Tiers                [][]Target // index 0 = highest priority (tried first)
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
	st      *store.Store
	rr      sync.Map                  // alias -> *atomic.Uint64
	aliases *lookupcache.Cache[*Plan] // nil = every Resolve asks the database
}

// NewRouter returns a Router backed by the store.
func NewRouter(st *store.Store) *Router { return &Router{st: st} }

// NewCachedRouter returns a Router that caches alias plans (see lookupcache).
// A cached Plan is shared between requests and must not be modified.
func NewCachedRouter(st *store.Store, opts lookupcache.Options) *Router {
	return &Router{st: st, aliases: lookupcache.New[*Plan](opts)}
}

// PurgeCache forgets every cached alias plan.
func (r *Router) PurgeCache() { r.aliases.Purge() }

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
		// The client already named the provider/model explicitly, so echoing
		// it back in headers reveals nothing new — same reasoning as the
		// DLPModelScan default above.
		return &Plan{Alias: model, Strategy: "round_robin", DLPModelScan: true, ExposeBackendHeaders: true, DLPAudioScan: true, Tiers: [][]Target{{t}}}, nil
	}
	return r.aliases.Get(ctx, model, func(ctx context.Context) (*Plan, error) {
		return r.resolveAlias(ctx, model)
	})
}

// resolveAlias expands an alias from the catalog into priority tiers.
func (r *Router) resolveAlias(ctx context.Context, model string) (*Plan, error) {
	var strategy string
	var dlpModelScan, exposeBackendHeaders, dlpAudioScan bool
	err := r.st.PG.QueryRow(ctx, `SELECT strategy, dlp_model_scan, expose_backend_headers, dlp_audio_scan FROM model_aliases WHERE alias = $1`, model).Scan(&strategy, &dlpModelScan, &exposeBackendHeaders, &dlpAudioScan)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, lookupcache.Miss(modelNotFoundError{model})
		}
		return nil, err
	}

	rows, err := r.st.PG.Query(ctx, `
		SELECT t.priority, t.provider_name, t.upstream_model, p.kind, t.display_label, t.options
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
		var kind string
		var t Target
		var options []byte
		if err := rows.Scan(&priority, &t.Provider, &t.UpstreamModel, &kind, &t.DisplayLabel, &options); err != nil {
			return nil, err
		}
		// The admin API validates options on save, so a parse failure here
		// means a hand-edited row. Serving the target under the gateway
		// defaults beats failing every request for the alias.
		if t.Options, err = ParseTargetOptions(options); err != nil {
			slog.Warn("routing: ignoring invalid target options", "alias", model, "provider", t.Provider, "err", err)
			t.Options = TargetOptions{}
		}
		// UpstreamProtocol is derived from the provider's own kind, the same
		// way passthroughTarget below does it — never operator-chosen. Every
		// registered kind (openai/openrouter/xai/groq/ollama/vertex/...)
		// currently speaks OpenAI wire format regardless of what a human
		// might have picked per-target; "anthropic" only means anything once
		// a real Anthropic-speaking provider kind exists.
		t.UpstreamProtocol = "openai"
		if kind == "anthropic" {
			t.UpstreamProtocol = "anthropic"
		}
		t.Tier = priority
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
		return nil, lookupcache.Miss(fmt.Errorf("model %q has no available targets", model))
	}
	return &Plan{Alias: model, Strategy: strategy, DLPModelScan: dlpModelScan, ExposeBackendHeaders: exposeBackendHeaders, DLPAudioScan: dlpAudioScan, Tiers: tiers}, nil
}

func (r *Router) passthroughTarget(ctx context.Context, provider, upstreamModel string) (Target, error) {
	if upstreamModel == "" {
		return Target{}, fmt.Errorf("explicit model %q missing upstream name", provider+"/")
	}
	var kind string
	var enabled bool
	err := r.st.PG.QueryRow(ctx,
		`SELECT kind, enabled FROM providers WHERE name = $1`, provider,
	).Scan(&kind, &enabled)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Target{}, fmt.Errorf("provider %q not found", provider)
		}
		return Target{}, err
	}
	if !enabled {
		// A passthrough-capable key already knows the provider's name (it had
		// to supply it); "not found" would be misleading when it in fact
		// exists but an admin disabled it — the same pgx.ErrNoRows branch
		// above used to cover both cases identically.
		return Target{}, fmt.Errorf("provider %q is disabled", provider)
	}
	proto := "openai"
	if kind == "anthropic" {
		proto = "anthropic"
	}
	return Target{Provider: provider, UpstreamModel: upstreamModel, UpstreamProtocol: proto}, nil
}
