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

	"github.com/ipsupport-llc/ipsupport-airllm/internal/providers"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/routing"
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
}

// loadFailover reads the failover defaults from settings into the atomic
// cache. A missing or unreadable row leaves the zero value in place.
func (s *Server) loadFailover(ctx context.Context) {
	var cfg failoverConfig
	if raw, err := s.st.GetSetting(ctx, "failover"); err == nil && len(raw) > 0 {
		_ = json.Unmarshal(raw, &cfg)
	}
	if cfg.TimeoutMS < 0 {
		cfg.TimeoutMS = 0
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

// errCodeTierTimeout marks an attempt abandoned because it outlived its
// target's time budget. Retryable, so it falls through like any upstream
// failure would.
const errCodeTierTimeout = "tier_timeout"

// Attempt states for runAttempt's budget race.
const (
	attemptPending int32 = iota
	attemptCommitted
	attemptExpired
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
	if err != nil && state.Load() == attemptExpired && ctx.Err() == nil {
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
// request (or, on failure, the last one attempted) and how many upstream
// calls it took. Both go to the usage ledger.
type execResult struct {
	routing.Target
	Attempts int
}

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
// started reports that a streaming call committed output, after which no
// error can be recovered by another target. A cancellation of ctx itself —
// the client went away — is never a target failure: it ends the request
// without trying anything else.
func (s *Server) executePlan(ctx context.Context, plan *routing.Plan, supports func(providers.Provider) error, call attemptCall) (res execResult, started bool, err error) {
	reg := s.reg()
	free := s.freeFunc(reg)
	session := clientSessionFrom(ctx)
	var lastErr error

	for retry := 0; retry <= busyRetries; retry++ {
		anyBusy := false
		for _, t := range plan.Ordered(s.router.NextRR(plan.Alias), free) {
			res.Target = t
			e, ok := reg.Get(t.Provider)
			if !ok {
				warnUnregisteredTarget(plan.Alias, t.Provider)
				lastErr = fmt.Errorf("provider %q not registered", t.Provider)
				continue
			}
			if supports != nil {
				if err := supports(e.Provider); err != nil {
					lastErr = err
					continue
				}
			}
			if !e.Acquire() {
				anyBusy = true
				continue
			}
			pol := s.policyFor(t)
			res.Attempts++
			began := time.Now()
			committed, callErr := runAttempt(ctx, t.Provider, pol.budget, func(actx context.Context, commit func() bool) error {
				return call(actx, e.Provider, t, commit)
			})
			e.Release()

			if callErr == nil {
				return res, committed, nil
			}
			if committed {
				return res, true, callErr
			}
			if ctx.Err() != nil {
				return res, false, callErr
			}
			lastErr = callErr
			logAttemptFailure(plan.Alias, t, callErr, time.Since(began), session)
			if !pol.fallsThrough(callErr) {
				return res, false, callErr
			}
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
	if lastErr == nil {
		lastErr = errAllBusy
	}
	if errors.Is(lastErr, errAllBusy) {
		s.metrics.IncRateLimited("provider_busy")
	}
	return res, false, lastErr
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

// clientSessionHeader is the optional request header a client uses to tie
// its requests together (e.g. every turn of one phone call). The gateway
// only logs it today.
const clientSessionHeader = "X-Session-Id"

// maxClientSessionLen bounds what a client can make the gateway log.
const maxClientSessionLen = 128

type clientSessionKey struct{}

// withClientSession carries the request's session header, if any, on ctx.
func withClientSession(ctx context.Context, r *http.Request) context.Context {
	id := r.Header.Get(clientSessionHeader)
	if id == "" {
		return ctx
	}
	if len(id) > maxClientSessionLen {
		id = id[:maxClientSessionLen]
	}
	return context.WithValue(ctx, clientSessionKey{}, id)
}

// clientSessionFrom returns the session withClientSession stored, or "".
func clientSessionFrom(ctx context.Context) string {
	id, _ := ctx.Value(clientSessionKey{}).(string)
	return id
}
