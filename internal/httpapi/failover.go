package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/breaker"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/providers"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/routing"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/unavail"
)

// failoverConfig is the gateway-wide failover policy (settings name
// "failover"). Each value is the default for a target whose own options do
// not set it; the zero value is the behaviour from before tier options
// existed, so an install that never saves this sees no change.
type failoverConfig struct {
	// TimeoutMS is the default per-target time budget; 0 means none.
	TimeoutMS int `json:"timeout_ms"`
	// FallbackOnAuth is the default for routing.TargetOptions.FallbackOnAuth.
	FallbackOnAuth bool `json:"fallback_on_auth"`
	// Breaker holds the default circuit breaker knobs; an unset one takes
	// breaker.Defaults. The breaker is off unless enabled here or per tier.
	Breaker routing.BreakerOptions `json:"breaker"`
	// UnavailableInitialMS/UnavailableMaxMS configure the backoff
	// unavail.Store falls back to when a tier-attributable failure carries
	// no Retry-After header: the first mark uses UnavailableInitialMS,
	// each repeat doubles, capped at UnavailableMaxMS. Unlike Breaker, this
	// is always on — see unavail.Store's own doc comment for why marking a
	// specific (provider, model) unavailable for a bounded, vendor-informed
	// span is safe to default on rather than requiring opt-in. <= 0 (unset,
	// including a never-saved settings row) takes the built-in default.
	UnavailableInitialMS int `json:"unavailable_initial_ms"`
	UnavailableMaxMS     int `json:"unavailable_max_ms"`
}

// Built-in defaults for the fields above when unset: 200ms doubling to a
// ~25.6s ceiling (8 doublings) — fast enough that a request-driven retry
// probes again almost immediately, bounded so a persistently failing
// target isn't retried faster than roughly once every half-minute once it
// settles at the ceiling.
const (
	defaultUnavailableInitialMS = 200
	defaultUnavailableMaxMS     = 25600
)

// loadFailover reads the failover defaults from settings into the atomic
// cache. A missing or unreadable row leaves the zero value in place.
func (s *Server) loadFailover(ctx context.Context) { s.loadSetting(ctx, "failover") }

// applyFailover installs the failover defaults from a raw settings value (nil = defaults).
func (s *Server) applyFailover(raw []byte) {
	var cfg failoverConfig
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &cfg)
	}
	if cfg.TimeoutMS < 0 {
		cfg.TimeoutMS = 0
	}
	if cfg.Breaker.Validate() != nil {
		cfg.Breaker = routing.BreakerOptions{}
	}
	if cfg.UnavailableInitialMS <= 0 {
		cfg.UnavailableInitialMS = defaultUnavailableInitialMS
	}
	if cfg.UnavailableMaxMS <= 0 {
		cfg.UnavailableMaxMS = defaultUnavailableMaxMS
	}
	s.failoverPtr.Store(&cfg)
}

// failoverCfg returns the current failover defaults.
func (s *Server) failoverCfg() failoverConfig {
	if c := s.failoverPtr.Load(); c != nil {
		return *c
	}
	return failoverConfig{}
}

// handleAdminGetFailover returns the gateway-wide failover defaults.
func (s *Server) handleAdminGetFailover(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.failoverCfg())
}

// handleAdminPutFailover saves the gateway-wide failover defaults.
func (s *Server) handleAdminPutFailover(w http.ResponseWriter, r *http.Request) {
	sess, _ := sessionFrom(r.Context())
	var body failoverConfig
	if err := decodeJSON(r, &body); err != nil {
		writeControlError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if body.TimeoutMS < 0 {
		writeControlError(w, http.StatusBadRequest, "timeout_ms must not be negative")
		return
	}
	if err := body.Breaker.Validate(); err != nil {
		writeControlError(w, http.StatusBadRequest, err.Error())
		return
	}
	raw, _ := json.Marshal(body)
	if err := s.st.PutSetting(r.Context(), "failover", raw); err != nil {
		writeControlError(w, http.StatusInternalServerError, "failed to save failover config")
		return
	}
	s.loadFailover(r.Context())
	s.audit(r.Context(), sess.principal.Subject, "failover.put", "failover", body)
	writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
}

// attemptPolicy is the failover policy one attempt runs under: the target's
// own options over the gateway-wide defaults.
type attemptPolicy struct {
	budget         time.Duration
	fallbackOnAuth bool
}

// policyFor resolves target t's policy key by key: a value set in its own
// options wins, an unset one takes the gateway-wide default.
func (s *Server) policyFor(t routing.Target) attemptPolicy {
	cfg := s.failoverCfg()
	timeoutMS := cfg.TimeoutMS
	if t.Options.TimeoutMS != nil {
		timeoutMS = *t.Options.TimeoutMS
	}
	fallbackOnAuth := cfg.FallbackOnAuth
	if t.Options.FallbackOnAuth != nil {
		fallbackOnAuth = *t.Options.FallbackOnAuth
	}
	return attemptPolicy{budget: time.Duration(timeoutMS) * time.Millisecond, fallbackOnAuth: fallbackOnAuth}
}

// fallsThrough reports whether a failed attempt should move on to the next
// target rather than fail the whole request.
func (p attemptPolicy) fallsThrough(err error) bool {
	if providers.IsFallbackWorthy(err) {
		return true
	}
	return p.fallbackOnAuth && providers.IsAuthFailure(err)
}

// errServedWithoutCall is what an attemptCall returns when it answered from a
// local store (the synthesis cache) instead of calling its target. The
// request is served, but nothing was learnt about the tier and no upstream
// call was made: no breaker verdict, no tier outcome, no counted attempt.
var errServedWithoutCall = errors.New("served without an upstream call")

// errCodeTierTimeout marks an attempt abandoned because it outlived its
// target's time budget. Retryable, so it falls through like any upstream
// failure would.
const errCodeTierTimeout = "tier_timeout"

// Attempt states for runAttempt's budget race. Whichever of the timer, a
// first chunk and the call's return moves the state off pending first wins.
const (
	attemptPending int32 = iota
	attemptCommitted
	attemptExpired
	attemptReturned
)

// runAttempt runs one upstream call under a time budget. call receives a
// context the budget cancels and a commit function a streaming call invokes
// on its first chunk: once committed, the budget no longer applies, and if
// the budget ran out first commit reports false and the call must not emit
// anything. A unary call never commits, so the budget bounds all of it. An
// error from a call whose budget ran out is replaced by a tier_timeout
// error; a cancellation of the caller's own context is left as it is.
func runAttempt(ctx context.Context, provider string, budget time.Duration, call func(ctx context.Context, commit func() bool) error) (committed bool, err error) {
	actx, cancel := context.WithCancel(ctx)
	defer cancel()
	var state atomic.Int32
	if budget > 0 {
		timer := time.AfterFunc(budget, func() {
			if state.CompareAndSwap(attemptPending, attemptExpired) {
				cancel()
			}
		})
		defer timer.Stop()
	}
	commit := func() bool {
		return state.CompareAndSwap(attemptPending, attemptCommitted) || state.Load() == attemptCommitted
	}
	err = call(actx, commit)
	// Claim the outcome before looking at it: a budget that runs out after
	// the call already returned must not relabel that call's own error.
	returnedFirst := state.CompareAndSwap(attemptPending, attemptReturned)
	if err != nil && !returnedFirst && state.Load() == attemptExpired && ctx.Err() == nil {
		err = &providers.Error{
			Status:    http.StatusGatewayTimeout,
			Retryable: true,
			Code:      errCodeTierTimeout,
			Message:   fmt.Sprintf("upstream %s did not answer within %s", provider, budget),
		}
	}
	return state.Load() == attemptCommitted, err
}

// execResult is what executing a plan did: the target that served the
// request (or, on failure, the last one attempted), how many upstream calls
// it took and the client session it belonged to. All go to the usage ledger.
type execResult struct {
	routing.Target
	Attempts int
	// Session is the request's client session header, or "".
	Session string
	// Cache is how the synthesis cache answered a speech request on an
	// alias that has it. Empty otherwise.
	Cache cacheOutcome
}

// cacheOutcome is how the synthesis cache answered one speech request,
// spelled as the request log line and the airllm_synthesis_cache_total
// metric spell it.
type cacheOutcome string

const (
	cacheHit   cacheOutcome = "hit"
	cacheMiss  cacheOutcome = "miss"
	cacheError cacheOutcome = "error" // the cache could not be read; a provider answered
)

// attemptCall makes one upstream call to target t through provider p. commit
// is runAttempt's: a streaming call invokes it before emitting its first
// chunk and stops if it reports false.
type attemptCall func(ctx context.Context, p providers.Provider, t routing.Target, commit func() bool) error

// executePlan walks the plan's targets in try-order, acquiring a concurrency
// slot per attempt and running each under its target's time budget. A busy
// target is skipped; a failure the target's policy deems fallback-worthy
// moves on to the next target; if every target was busy it waits briefly and
// retries. supports, when non-nil, rejects targets whose provider lacks the
// capability the call needs; they are skipped with unsupported as the error.
//
// Each tier sits behind its circuit breaker: an open tier is skipped without
// a call, and every attempt's outcome is recorded against its tier. When the
// only targets left were in open tiers the request does not fail at once: it
// forces the probe of the open tier whose cooldown ends first (lowest
// priority on a tie), so tiers that tripped together on a gateway-side blip
// do not keep the alias down for a whole cooldown. Only when that tier is
// already being probed, or the forced attempt makes no call, does the
// request fail fast with errAllQuarantined.
//
// On an alias with session affinity, a session that a backup tier served
// starts at that tier: earlier tiers are not tried, later ones still are.
//
// started reports that a streaming call committed output, after which no
// error can be recovered by another target. A cancellation of ctx itself —
// the client went away — is never a target failure: it ends the request
// without trying anything else.
func (s *Server) executePlan(ctx context.Context, plan *routing.Plan, supports func(providers.Provider) error, call attemptCall) (res execResult, started bool, err error) {
	reg := s.reg()
	free := s.freeFunc(reg)
	session := clientSessionFrom(ctx)
	res.Session = session
	floor, pinned := s.sessionFloor(ctx, plan, session)
	var lastErr error
	// skipErr is why a target was skipped as unable to serve the call. It
	// is the answer only when nothing else happened: a capable target that
	// was merely busy makes the request a 429, not a "does not support".
	var skipErr error
	// moved is every tier this request went past, for the fallback metric;
	// quarantined is whether any of them was skipped by its breaker, and
	// unavailable whether any target was skipped as marked down.
	var moved []tierFallback
	quarantined, unavailable := false, false
	// openUntil is when the cooldown of each open tier the breaker turned
	// this request away from runs out: the candidates for a forced probe.
	openUntil := map[int]time.Time{}
	settings := map[int]breaker.Settings{}
	tierSettings := func(tier int) breaker.Settings {
		set, ok := settings[tier]
		if !ok {
			set = s.breakerSettings(plan, tier)
			settings[tier] = set
		}
		return set
	}
	defer func() {
		var served *int
		if err == nil {
			served = &res.Tier
		}
		s.recordTierFallbacks(plan.Alias, moved, served)
		if err == nil || started {
			s.pinSession(ctx, plan, session, res.Tier)
		}
	}()

	sawBusy := false
	// forced is the tier the last-resort pass forces a probe on, -1 on the
	// first pass; forcedCall is whether that pass made its one call.
	forced, forcedCall := -1, false
	for pass := 0; pass < 2; pass++ {
		if pass == 1 {
			// Only a request that made no call and found no busy
			// target is out of options because of the breaker.
			if sawBusy || res.Attempts > 0 {
				break
			}
			tier, ok := soonestOpen(openUntil)
			if !ok {
				break
			}
			forced = tier
		}
		for retry := 0; retry <= busyRetries; retry++ {
			anyBusy := false
			for _, t := range plan.Ordered(s.router.NextRR(plan.Alias), free) {
				if pinned && t.Tier < floor {
					continue
				}
				if forced >= 0 && (t.Tier != forced || forcedCall) {
					continue
				}
				res.Target = t
				e, ok := reg.Get(t.Provider)
				if !ok {
					warnUnregisteredTarget(plan.Alias, t.Provider)
					lastErr = fmt.Errorf("provider %q not registered", t.Provider)
					continue
				}
				if supports != nil {
					if err := supports(e.Provider); err != nil {
						skipErr = err
						continue
					}
				}
				pol := s.policyFor(t)
				key, set := breaker.Key{Alias: plan.Alias, Tier: t.Tier}, tierSettings(t.Tier)
				// A probe holds the tier for as long as its attempt may run.
				set.ProbeLease = pol.budget + time.Second
				tierLabel := strconv.Itoa(t.Tier)
				var adm breaker.Admission
				if forced >= 0 {
					adm = s.breaker.Force(ctx, key, set)
				} else {
					adm = s.breaker.Admit(ctx, key, set)
				}
				if adm.Skip {
					if forced >= 0 {
						// Another request holds the probe; this one was
						// already counted as skipping the tier.
						continue
					}
					quarantined = true
					moved = append(moved, tierFallback{tier: t.Tier, reason: "quarantined"})
					s.metrics.TierSkipped(plan.Alias, tierLabel, "quarantined")
					if !adm.OpenUntil.IsZero() {
						openUntil[t.Tier] = adm.OpenUntil
					}
					continue
				}
				unavailKey := unavail.Key{Provider: t.Provider, UpstreamModel: t.UpstreamModel}
				if _, skip := s.unavail.Check(ctx, unavailKey); skip {
					// A prior attempt at this exact (provider, model) — from
					// ANY alias/tier that references it — already learned it's
					// down, within the window it (or our own default backoff)
					// said to wait. No call is made; the next real request
					// past that window is itself the retry (see internal/
					// unavail's doc comment for why there's no separate
					// prober).
					if adm.Probe {
						s.breaker.AbandonProbe(context.WithoutCancel(ctx), key, set)
					}
					unavailable = true
					moved = append(moved, tierFallback{tier: t.Tier, reason: "unavailable"})
					s.metrics.TierSkipped(plan.Alias, tierLabel, "unavailable")
					continue
				}
				if !e.Acquire() {
					if adm.Probe {
						s.breaker.AbandonProbe(context.WithoutCancel(ctx), key, set)
					}
					anyBusy, sawBusy = true, true
					continue
				}
				res.Attempts++
				forcedCall = forced >= 0
				began := time.Now()
				// A stream that delivers its first chunk has answered: that is
				// the tier's success, recorded then rather than when a long
				// stream finally ends, so a probe resolves at once.
				var recorded atomic.Bool
				recordSuccess := func() {
					if recorded.CompareAndSwap(false, true) {
						s.breaker.Record(ctx, key, set, false, adm.Probe)
						s.metrics.TierAttempt(plan.Alias, tierLabel, "success", time.Since(began))
					}
				}
				committed, callErr := runAttempt(ctx, t.Provider, pol.budget, func(actx context.Context, commit func() bool) error {
					return call(actx, e.Provider, t, func() bool {
						ok := commit()
						if ok {
							recordSuccess()
						}
						return ok
					})
				})
				e.Release()

				if errors.Is(callErr, errServedWithoutCall) {
					if adm.Probe {
						s.breaker.AbandonProbe(ctx, key, set)
					}
					res.Attempts--
					return res, committed, nil
				}
				switch {
				case recorded.Load():
				case callErr == nil:
					recordSuccess()
				case ctx.Err() != nil:
					// The client went away: nothing was learnt about the tier.
					if adm.Probe {
						s.breaker.AbandonProbe(context.WithoutCancel(ctx), key, set)
					}
					return res, committed, callErr
				case countsAgainstTier(pol, callErr):
					s.breaker.Record(ctx, key, set, true, adm.Probe)
					s.metrics.TierAttempt(plan.Alias, tierLabel, "failure", time.Since(began))
					s.markUnavailable(ctx, unavailKey, callErr)
				default:
					// The request itself was the problem: no verdict on the tier.
					if adm.Probe {
						s.breaker.AbandonProbe(ctx, key, set)
					}
					s.metrics.TierAttempt(plan.Alias, tierLabel, "request_error", time.Since(began))
				}

				if callErr == nil {
					return res, committed, nil
				}
				if committed {
					return res, true, callErr
				}
				lastErr = callErr
				logAttemptFailure(plan.Alias, t, callErr, time.Since(began), session)
				if !pol.fallsThrough(callErr) {
					return res, false, callErr
				}
				moved = append(moved, tierFallback{tier: t.Tier, reason: attemptFailureReason(callErr)})
			}
			if !anyBusy {
				break
			}
			select {
			case <-ctx.Done():
				return res, false, ctx.Err()
			case <-time.After(busyBackoff):
			}
		}
	}
	if lastErr == nil {
		switch {
		case sawBusy:
			lastErr = errAllBusy
		case quarantined:
			lastErr = errAllQuarantined
		case unavailable:
			lastErr = errAllUnavailable
		case skipErr != nil:
			lastErr = skipErr
		default:
			lastErr = errAllBusy
		}
	}
	if errors.Is(lastErr, errAllBusy) {
		s.metrics.IncRateLimited("provider_busy")
	}
	return res, false, lastErr
}

// soonestOpen picks the open tier whose cooldown runs out first, the lowest
// priority on a tie.
func soonestOpen(openUntil map[int]time.Time) (int, bool) {
	tier, ok := 0, false
	for t, at := range openUntil {
		if !ok || at.Before(openUntil[tier]) || (at.Equal(openUntil[tier]) && t < tier) {
			tier, ok = t, true
		}
	}
	return tier, ok
}

// logAttemptFailure emits one line per failed upstream attempt, so an
// operator can see why a request moved on (or stopped) at each target.
func logAttemptFailure(alias string, t routing.Target, err error, latency time.Duration, session string) {
	attrs := []any{
		"alias", alias, "tier", t.Tier, "provider", t.Provider, "upstream_model", t.UpstreamModel,
		"reason", attemptFailureReason(err), "latency_ms", latency.Milliseconds(), "error", err.Error(),
	}
	if session != "" {
		attrs = append(attrs, "session", session)
	}
	slog.Warn("tier attempt failed", attrs...)
}

// markUnavailable marks k unavailable after a tier-attributable failure:
// the upstream's own Retry-After when callErr carries one, else the
// gateway's configured default backoff (doubling from this target's own
// last mark, capped). Logs the duration actually used, so an operator sees
// exactly how long a target is being skipped and why.
func (s *Server) markUnavailable(ctx context.Context, k unavail.Key, callErr error) {
	if s.unavail == nil {
		return
	}
	var pe *providers.Error
	var retryAfter *time.Duration
	if errors.As(callErr, &pe) {
		retryAfter = pe.RetryAfter
	}
	cfg := s.failoverCfg()
	initialMS, maxMS := cfg.UnavailableInitialMS, cfg.UnavailableMaxMS
	// loadFailover applies these same built-in defaults whenever settings
	// were never saved — but a *Server a test builds directly, bypassing
	// NewServer's init sequence, never calls it at all. Applying the
	// defaults here too means a real failure never marks a target
	// unavailable for a degenerate 0ms no matter how the Server was built.
	if initialMS <= 0 {
		initialMS = defaultUnavailableInitialMS
	}
	if maxMS <= 0 {
		maxMS = defaultUnavailableMaxMS
	}
	initial := time.Duration(initialMS) * time.Millisecond
	max := time.Duration(maxMS) * time.Millisecond
	dur := s.unavail.Mark(ctx, k, retryAfter, initial, max)
	slog.Warn("target marked unavailable", "provider", k.Provider, "upstream_model", k.UpstreamModel,
		"duration_ms", dur.Milliseconds(), "source", unavailSource(retryAfter))
}

// unavailSource names where markUnavailable's duration came from, for the
// log line above.
func unavailSource(retryAfter *time.Duration) string {
	if retryAfter != nil {
		return "retry_after"
	}
	return "default_backoff"
}

// attemptFailureReason is a short, stable label for why an attempt failed:
// the provider error code when there is one, else the upstream status class.
func attemptFailureReason(err error) string {
	var pe *providers.Error
	if !errors.As(err, &pe) {
		return "error"
	}
	switch {
	case pe.Code == errCodeTierTimeout:
		return "timeout"
	case pe.Code != "":
		return pe.Code
	case pe.Status == http.StatusTooManyRequests:
		return "rate_limited"
	default:
		return "http_" + strconv.Itoa(pe.Status)
	}
}
