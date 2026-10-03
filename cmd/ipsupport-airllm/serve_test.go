package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

// serveFixture runs serve on a loopback listener with a /slow route that
// answers only once release is closed, and a /fast one that answers at once.
type serveFixture struct {
	addr     string
	cancel   context.CancelFunc
	release  chan struct{}
	started  chan struct{} // a /slow request has reached the handler
	draining chan struct{} // serve reported the drain
	done     chan error    // serve's result
}

func startServe(t *testing.T, drain, timeout time.Duration) *serveFixture {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f := &serveFixture{
		addr:     ln.Addr().String(),
		release:  make(chan struct{}),
		started:  make(chan struct{}, 1),
		draining: make(chan struct{}),
		done:     make(chan error, 1),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		f.started <- struct{}{}
		<-f.release
		_, _ = io.WriteString(w, "done")
	})
	mux.HandleFunc("/fast", func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "ok") })
	ctx, cancel := context.WithCancel(context.Background())
	f.cancel = cancel
	go func() {
		f.done <- serve(ctx, &http.Server{Handler: mux}, ln, drain, timeout, func() { close(f.draining) })
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-f.release:
		default:
			close(f.release)
		}
	})
	return f
}

// get sends one request on a fresh connection.
func (f *serveFixture) get(path string) (string, error) {
	c := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}, Timeout: 5 * time.Second}
	resp, err := c.Get("http://" + f.addr + path)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return string(b), err
}

func waitFor(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func TestServeKeepsAcceptingThroughTheDrainAndFinishesInFlightRequests(t *testing.T) {
	const drain = 500 * time.Millisecond
	f := startServe(t, drain, 5*time.Second)

	slow := make(chan string, 1)
	go func() {
		body, err := f.get("/slow")
		if err != nil {
			body = "error: " + err.Error()
		}
		slow <- body
	}()
	waitFor(t, f.started, "the in-flight request")

	signalled := time.Now()
	f.cancel() // SIGTERM
	waitFor(t, f.draining, "the drain to start")
	if body, err := f.get("/fast"); err != nil || body != "ok" {
		t.Fatalf("new request during the drain = %q, %v; want ok", body, err)
	}

	time.Sleep(drain - time.Since(signalled) + 200*time.Millisecond)
	if _, err := f.get("/fast"); err == nil {
		t.Error("a new connection was accepted after the drain ended")
	}

	close(f.release)
	if body := <-slow; body != "done" {
		t.Errorf("in-flight request = %q, want done", body)
	}
	if err := <-f.done; err != nil {
		t.Errorf("serve = %v, want nil after a clean shutdown", err)
	}
}

func TestServeGivesUpOnRequestsThatOutlastTheTimeout(t *testing.T) {
	const timeout = 300 * time.Millisecond
	f := startServe(t, 0, timeout)
	go func() { _, _ = f.get("/slow") }()
	waitFor(t, f.started, "the in-flight request")

	signalled := time.Now()
	f.cancel()
	select {
	case err := <-f.done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("serve = %v, want a deadline error", err)
		}
		if elapsed := time.Since(signalled); elapsed > timeout+time.Second {
			t.Errorf("serve returned after %v, want about %v", elapsed, timeout)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not give up on the stuck request")
	}
}
