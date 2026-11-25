package main

import (
	"net/http"
	"sync/atomic"
)

// swapHandler serves through whichever handler was stored last. The listener
// opens before migrations run, behind bootHandler; run swaps in the real API
// handler once setup has finished, on the same http.Server.
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

// bootHandler answers while the gateway is still migrating and setting up:
// the process is alive (/healthz, so the startup and liveness probes pass
// however long a migration takes), but not ready (/readyz), and nothing else
// is served yet.
func bootHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeBootJSON(w, http.StatusOK, "starting")
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		writeBootJSON(w, http.StatusServiceUnavailable, "migrating")
	})
	return mux
}

func writeBootJSON(w http.ResponseWriter, code int, status string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(`{"status":"` + status + `"}` + "\n"))
}
