package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/breaker"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/metrics"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/providers"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/routing"
)

// errAllQuarantined is returned when every target that could have served
// the request sits in an open tier.
var errAllQuarantined = errors.New("every tier of this model is temporarily quarantined")

// newBreaker builds the tier breaker with this server's logs and metrics as
// its observer. rdb nil keeps breaker state in this process only.
func (s *Server) newBreaker(rdb *redis.Client, opts ...breaker.Option) *breaker.Breaker {
	return breaker.New(rdb, append([]breaker.Option{breaker.WithObserver(s.onBreakerTransition)}, opts...)...)
}

// onBreakerTransition logs and counts one breaker state change.
func (s *Server) onBreakerTransition(t breaker.Transition) {
	tier := strconv.Itoa(t.Key.Tier)
	s.metrics.BreakerTransition(t.Key.Alias, tier, string(t.To))
	switch t.To {
	case breaker.Open:
		slog.Warn("tier breaker opened", "alias", t.Key.Alias, "tier", t.Key.Tier, "reason", t.Cause,
			"cooldown_ms", t.Cooldown.Milliseconds(), "open_until", t.OpenUntil.UTC().Format(time.RFC3339))
	case breaker.HalfOpen:
		slog.Info("tier breaker probe", "alias", t.Key.Alias, "tier", t.Key.Tier)
	case breaker.Closed:
		slog.Info("tier breaker closed", "alias", t.Key.Alias, "tier", t.Key.Tier, "via", t.Cause)
	}
}

// breakerStates reads the state of every breaker this replica has used, for
// the state gauge.
func (s *Server) breakerStates() []metrics.BreakerState {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var out []metrics.BreakerState
	s.breaker.Seen(func(k breaker.Key, set breaker.Settings) {
		st, err := s.breaker.Status(ctx, k, set)
		if err != nil {
			return
		}
		v := 0.0
		switch st.State {
		case breaker.Open:
			v = 1
		case breaker.HalfOpen:
			v = 2
		}
		out = append(out, metrics.BreakerState{Alias: k.Alias, Tier: strconv.Itoa(k.Tier), Value: v})
	})
	return out
}

// breakerSettings resolves the breaker thresholds of one tier of plan, key
// by key: a value set on the tier wins over the gateway-wide default, which
// wins over breaker.Defaults. A tier's value comes from the first of its
// targets (by provider, then upstream model) that sets it, so the answer does
// not depend on the order the catalog returned them in.
func (s *Server) breakerSettings(plan *routing.Plan, tier int) breaker.Settings {
	var targets []routing.Target
	for _, ts := range plan.Tiers {
		for _, t := range ts {
			if t.Tier == tier && t.Options.Breaker != nil {
				targets = append(targets, t)
			}
		}
	}
	sort.SliceStable(targets, func(i, j int) bool {
		if targets[i].Provider != targets[j].Provider {
			return targets[i].Provider < targets[j].Provider
		}
		return targets[i].UpstreamModel < targets[j].UpstreamModel
	})
	layers := make([]*routing.BreakerOptions, 0, len(targets)+1)
	for _, t := range targets {
		layers = append(layers, t.Options.Breaker)
	}
	def := s.failoverCfg().Breaker
	layers = append(layers, &def)

	set := breaker.Defaults()
	pickBool := func(get func(*routing.BreakerOptions) *bool, dst *bool) {
		for _, l := range layers {
			if v := get(l); v != nil {
				*dst = *v
				return
			}
		}
	}
	pickInt := func(get func(*routing.BreakerOptions) *int, apply func(int)) {
		for _, l := range layers {
			if v := get(l); v != nil {
				apply(*v)
				return
			}
		}
	}
	ms := func(dst *time.Duration) func(int) {
		return func(v int) { *dst = time.Duration(v) * time.Millisecond }
	}
	pickBool(func(o *routing.BreakerOptions) *bool { return o.Enabled }, &set.Enabled)
	pickInt(func(o *routing.BreakerOptions) *int { return o.Failures }, func(v int) { set.Failures = v })
	pickInt(func(o *routing.BreakerOptions) *int { return o.MinRequests }, func(v int) { set.MinRequests = v })
	pickInt(func(o *routing.BreakerOptions) *int { return o.WindowMS }, ms(&set.Window))
	pickInt(func(o *routing.BreakerOptions) *int { return o.CooldownMS }, ms(&set.Cooldown))
	pickInt(func(o *routing.BreakerOptions) *int { return o.MaxCooldownMS }, ms(&set.MaxCooldown))
	pickInt(func(o *routing.BreakerOptions) *int { return o.StableMS }, ms(&set.Stable))
	for _, l := range layers {
		if l.ErrorRate != nil {
			set.ErrorRate = *l.ErrorRate
			break
		}
	}
	return set
}

// countsAgainstTier reports whether a failed attempt says something about the
// tier's health. A failure the request itself caused — its context is too
// long for this model, it carries images the model cannot read — does not:
// the tier would serve the next request fine.
func countsAgainstTier(pol attemptPolicy, err error) bool {
	if err == nil || !pol.fallsThrough(err) {
		return false
	}
	var pe *providers.Error
	if errors.As(err, &pe) {
		switch pe.Code {
		case providers.ErrCodeContextLengthExceeded, providers.ErrCodeMultimodalNotSupported, providers.ErrCodeReasoningEffortUnsupported:
			return false
		}
	}
	return true
}

// tierFallback is one tier a request moved past, and why.
type tierFallback struct {
	tier   int
	reason string
}

// recordTierFallbacks counts each tier the request moved past against the
// tier that finally served it ("none" when nothing did).
func (s *Server) recordTierFallbacks(alias string, moved []tierFallback, served *int) {
	to := "none"
	if served != nil {
		to = strconv.Itoa(*served)
	}
	seen := make(map[tierFallback]bool, len(moved))
	for _, f := range moved {
		if seen[f] || (served != nil && f.tier == *served) {
			continue
		}
		seen[f] = true
		s.metrics.TierFallback(alias, strconv.Itoa(f.tier), to, f.reason)
	}
}
