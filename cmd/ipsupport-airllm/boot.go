package main

import (
	"net/http"
	"sync/atomic"
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

// bootHandler answers while the gateway is still setting up. It never
// reports ready. /healthz passes only while migrations run (waiting for
// another replica's migration lock included), so the startup probe stops
// counting for as long as a migration takes; in every other boot phase it
// fails, and a hang there (a datastore that never answers, OIDC discovery)
// still runs out the startup probe's budget and restarts the pod.
type bootHandler struct {
	migrating atomic.Bool
}

// SetMigrating marks the start (true) and end (false) of migrations.
func (b *bootHandler) SetMigrating(on bool) { b.migrating.Store(on) }

func (b *bootHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !b.migrating.Load() {
		writeBootJSON(w, http.StatusServiceUnavailable, "starting")
		return
	}
	code := http.StatusServiceUnavailable
	if r.URL.Path == "/healthz" && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
		code = http.StatusOK
	}
	writeBootJSON(w, code, "migrating")
}

func writeBootJSON(w http.ResponseWriter, code int, status string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(`{"status":"` + status + `"}` + "\n"))
}
