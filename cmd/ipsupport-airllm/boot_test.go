package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBootHandlerIsAliveButNotReady(t *testing.T) {
	h := bootHandler()
	for _, tc := range []struct {
		method, path string
		code         int
		body         string
	}{
		{"GET", "/healthz", http.StatusOK, `{"status":"starting"}`},
		{"GET", "/readyz", http.StatusServiceUnavailable, `{"status":"migrating"}`},
		{"POST", "/v1/chat/completions", http.StatusServiceUnavailable, `{"status":"migrating"}`},
		{"GET", "/", http.StatusServiceUnavailable, `{"status":"migrating"}`},
		{"POST", "/healthz", http.StatusServiceUnavailable, `{"status":"migrating"}`},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != tc.code || strings.TrimSpace(rec.Body.String()) != tc.body {
			t.Errorf("%s %s = %d %q, want %d %s", tc.method, tc.path, rec.Code, rec.Body.String(), tc.code, tc.body)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("%s %s Content-Type = %q, want application/json", tc.method, tc.path, ct)
		}
	}
}

// TestSwapHandlerServesTheRealHandlerAfterTheSwap runs one http.Server the
// way run does: boot handler first, then the API handler swapped in while
// the server keeps running — including on a kept-alive connection opened
// before the swap.
func TestSwapHandlerServesTheRealHandlerAfterTheSwap(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	handler := newSwapHandler(bootHandler())
	srv := &http.Server{Handler: handler}
	errCh := startServing(srv, ln)
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
	if err := serve(ctx, srv, errCh, 0, time.Second, func() {}); err != nil {
		t.Errorf("serve = %v, want a clean shutdown", err)
	}
}
