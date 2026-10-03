package httpapi

import (
	"context"
	"log/slog"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/routing"
)

// sessionFloor is the earliest tier a request of session may use on plan:
// the tier the session is pinned to. pinned is false when the alias has
// affinity off, the request carries no session, or the session holds no
// pin. A pin to a tier the plan no longer reaches — it and every later tier
// were removed or disabled since — is ignored: starting the session over
// beats failing the rest of the call.
func (s *Server) sessionFloor(ctx context.Context, plan *routing.Plan, session string) (floor int, pinned bool) {
	if !plan.SessionAffinity || session == "" {
		return 0, false
	}
	floor, pinned = s.affinity.Pinned(ctx, plan.Alias, session)
	if !pinned {
		return 0, false
	}
	for _, ts := range plan.Tiers {
		for _, t := range ts {
			if t.Tier >= floor {
				return floor, true
			}
		}
	}
	return 0, false
}

// pinSession records that tier served a request of session on plan, when
// that tier is a backup one, so the session's later requests start there.
// Serving it again renews the pin's TTL.
func (s *Server) pinSession(ctx context.Context, plan *routing.Plan, session string, tier int) {
	if !plan.SessionAffinity || session == "" || len(plan.Tiers) == 0 || len(plan.Tiers[0]) == 0 {
		return
	}
	if tier <= plan.Tiers[0][0].Tier {
		return
	}
	ttl := plan.SessionAffinityTTL
	if ttl <= 0 {
		ttl = routing.DefaultAffinityTTL
	}
	if s.affinity.Pin(context.WithoutCancel(ctx), plan.Alias, session, tier, ttl) {
		slog.Info("session pinned to tier", "alias", plan.Alias, "session", session, "tier", tier, "ttl_s", int(ttl.Seconds()))
	}
}
