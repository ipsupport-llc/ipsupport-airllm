package main

import (
	"net/http"
	"sync/atomic"
	"time"
)

// swapHandler serves through whichever handler was stored last. The listener
// opens before migrations run, behind a bootHandler; run swaps in the real
// API handler once setup has finished, on the same http.Server.
type swapHandler struct {
	h atomic.Pointer[http.Handler]
}

func newSwapHandler(h http.Handler) *swapHandler {
	s := &swapHandler{}
	s.Store(h)
	return s
}

// Store makes h serve every request from now on.
func (s *swapHandler) Store(h http.Handler) { s.h.Store(&h) }

func (s *swapHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	(*s.h.Load()).ServeHTTP(w, r)
}

// postMigrationGrace keeps /healthz passing for a while after migrations
// finish. A long migration satisfies the startup probe, after which only
// the liveness probe (~30s) would watch the rest of setup; the grace gives
// that the same ~60s the startup probe allows.
const postMigrationGrace = 60 * time.Second

// bootHandler answers while the gateway is still setting up. It never
// reports ready. /healthz passes only while migrations run — from the moment
// the migration lock is held, so a lock holder wedged elsewhere still runs
// out this pod's startup probe — and for postMigrationGrace after. In every
// other boot phase it fails, so a hang there (a datastore that never
// answers, OIDC discovery) restarts the pod as before.
type bootHandler struct {
	now func() time.Time // the clock; nil means time.Now
	// phase switches in one atomic store, so /healthz has no gap between
	// migrating and the grace period. nil: not live.
	phase atomic.Pointer[bootPhase]
}

type bootPhase struct {
	migrating bool
	// doneAt is when migrations succeeded. A time.Time from time.Now keeps
	// its monotonic reading, so a wall-clock step does not stretch or cut
	// the grace period.
	doneAt time.Time
}

// MigrationStarted marks the migration lock as held.
func (b *bootHandler) MigrationStarted() { b.phase.Store(&bootPhase{migrating: true}) }

// MigrationFinished ends the migrating phase. Only migrations that
// succeeded (ok) after MigrationStarted start the grace period; after a
// failure the process is about to exit.
func (b *bootHandler) MigrationFinished(ok bool) {
	if p := b.phase.Load(); ok && p != nil && p.migrating {
		b.phase.Store(&bootPhase{doneAt: b.clock()})
		return
	}
	b.phase.Store(nil)
}

func (b *bootHandler) clock() time.Time {
	if b.now != nil {
		return b.now()
	}
	return time.Now()
}

func (b *bootHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	status, live := "starting", false
	if p := b.phase.Load(); p != nil && p.migrating {
		status, live = "migrating", true
	} else if p != nil && b.clock().Sub(p.doneAt) < postMigrationGrace {
		live = true
	}
	code := http.StatusServiceUnavailable
	if live && r.URL.Path == "/healthz" && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
		code = http.StatusOK
	}
	writeBootJSON(w, code, status)
}

func writeBootJSON(w http.ResponseWriter, code int, status string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(`{"status":"` + status + `"}` + "\n"))
}
