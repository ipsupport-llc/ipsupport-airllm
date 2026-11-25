package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBootHandlerPhases(t *testing.T) {
	type want struct {
		method, path string
		code         int
		body         string
	}
	starting := []want{
		// Not live: a hang outside migrations runs out the startup probe.
		{"GET", "/healthz", http.StatusServiceUnavailable, `{"status":"starting"}`},
		{"GET", "/readyz", http.StatusServiceUnavailable, `{"status":"starting"}`},
		{"POST", "/v1/chat/completions", http.StatusServiceUnavailable, `{"status":"starting"}`},
	}
	grace := []want{
		// Live but not ready: the rest of setup keeps a ~60s budget.
		{"GET", "/healthz", http.StatusOK, `{"status":"starting"}`},
		{"GET", "/readyz", http.StatusServiceUnavailable, `{"status":"starting"}`},
		{"POST", "/healthz", http.StatusServiceUnavailable, `{"status":"starting"}`},
	}
	migrating := []want{
		{"GET", "/healthz", http.StatusOK, `{"status":"migrating"}`},
		{"HEAD", "/healthz", http.StatusOK, ""},
		{"GET", "/readyz", http.StatusServiceUnavailable, `{"status":"migrating"}`},
		{"POST", "/v1/chat/completions", http.StatusServiceUnavailable, `{"status":"migrating"}`},
		{"GET", "/", http.StatusServiceUnavailable, `{"status":"migrating"}`},
		{"POST", "/healthz", http.StatusServiceUnavailable, `{"status":"migrating"}`},
	}
	check := func(phase string, h http.Handler, cases []want) {
		t.Helper()
		for _, tc := range cases {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
			body := strings.TrimSpace(rec.Body.String())
			if tc.method == "HEAD" {
				body = "" // the server drops a HEAD body; only the code matters
			}
			if rec.Code != tc.code || body != tc.body {
				t.Errorf("%s: %s %s = %d %q, want %d %s", phase, tc.method, tc.path, rec.Code, body, tc.code, tc.body)
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("%s: %s %s Content-Type = %q, want application/json", phase, tc.method, tc.path, ct)
			}
		}
	}

	now := time.Unix(1_800_000_000, 0)
	b := &bootHandler{now: func() time.Time { return now }}
	check("before the migration lock", b, starting)
	b.MigrationStarted()
	check("migrating", b, migrating)
	now = now.Add(10 * time.Minute) // a long migration: no time limit
	check("still migrating", b, migrating)
	b.MigrationFinished()
	check("right after migrations", b, grace)
	now = now.Add(postMigrationGrace - time.Second)
	check("end of the grace period", b, grace)
	now = now.Add(time.Second)
	check("after the grace period", b, starting)

	// A Migrate that fails before taking the lock never went live.
	failed := &bootHandler{now: func() time.Time { return now }}
	failed.MigrationFinished()
	check("migration never started", failed, starting)
}

// TestSwapHandlerServesTheRealHandlerAfterTheSwap runs one http.Server the
// way run does: boot handler first, then the API handler swapped in while
// the server keeps running.
func TestSwapHandlerServesTheRealHandlerAfterTheSwap(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	boot := &bootHandler{}
	boot.MigrationStarted()
	handler := newSwapHandler(boot)
	srv := &http.Server{Handler: handler}
	sv := startServing(srv, ln)
	t.Cleanup(func() { _ = srv.Close() })

	c := &http.Client{Timeout: 5 * time.Second}
	get := func(path string) (int, string) {
		t.Helper()
		resp, err := c.Get("http://" + ln.Addr().String() + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, strings.TrimSpace(string(b))
	}

	if code, body := get("/readyz"); code != http.StatusServiceUnavailable || body != `{"status":"migrating"}` {
		t.Fatalf("before swap: /readyz = %d %s", code, body)
	}

	api := http.NewServeMux()
	api.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "real ready") })
	api.HandleFunc("GET /v1/models", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "real models") })
	handler.Store(api)

	if code, body := get("/readyz"); code != http.StatusOK || body != "real ready" {
		t.Errorf("after swap: /readyz = %d %s, want 200 real ready", code, body)
	}
	if code, body := get("/v1/models"); code != http.StatusOK || body != "real models" {
		t.Errorf("after swap: /v1/models = %d %s, want 200 real models", code, body)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := serve(ctx, srv, sv, 0, time.Second, func() {}); err != nil {
		t.Errorf("serve = %v, want a clean shutdown", err)
	}
}

// TestServeFailureCancelsSetup: if the server stops during setup, the setup
// context is cancelled with the Serve error as its cause, so a running
// migration aborts instead of finishing behind a dead listener.
func TestServeFailureCancelsSetup(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: &bootHandler{}}
	sv := startServing(srv, ln)
	t.Cleanup(func() { _ = srv.Close() })
	ctx, cancel := cancelOnServeFailure(context.Background(), sv)
	defer cancel()

	select {
	case <-ctx.Done():
		t.Fatal("setup context cancelled while the server is running")
	case <-time.After(50 * time.Millisecond):
	}

	_ = ln.Close() // Serve fails: not a Shutdown or Close
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("setup context not cancelled after the server stopped")
	}
	if cause := context.Cause(ctx); cause == nil || !errors.Is(cause, sv.err) || errors.Is(cause, context.Canceled) {
		t.Errorf("cause = %v, want the Serve error %v", cause, sv.err)
	}
	if err := serve(context.Background(), srv, sv, 0, time.Second, func() {}); err == nil || err != sv.err {
		t.Errorf("serve after the failure = %v, want %v", err, sv.err)
	}
}

// A clean shutdown is not a failure: the setup context stays alive.
func TestServeShutdownDoesNotCancelSetup(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: &bootHandler{}}
	sv := startServing(srv, ln)
	ctx, cancel := cancelOnServeFailure(context.Background(), sv)
	defer cancel()
	if err := srv.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	select {
	case <-ctx.Done():
		t.Fatalf("setup context cancelled by Close: %v", context.Cause(ctx))
	case <-time.After(100 * time.Millisecond):
	}
}
