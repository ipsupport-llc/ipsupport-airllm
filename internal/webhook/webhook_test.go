package webhook

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestIsDisallowedWebhookIP(t *testing.T) {
	cases := []struct {
		name string
		ip   string
		want bool
	}{
		{"loopback v4", "127.0.0.1", true},
		{"loopback v6", "::1", true},
		{"rfc1918 10/8", "10.1.2.3", true},
		{"rfc1918 192.168/16", "192.168.1.1", true},
		{"rfc1918 172.16/12", "172.16.0.1", true},
		{"link-local incl. cloud metadata", "169.254.169.254", true},
		{"unspecified v4", "0.0.0.0", true},
		{"multicast", "224.0.0.1", true},
		{"public v4", "8.8.8.8", false},
		{"public v4 doc range", "203.0.113.1", false},
		{"public v6", "2606:4700:4700::1111", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ip := net.ParseIP(c.ip)
			if ip == nil {
				t.Fatalf("bad test IP %q", c.ip)
			}
			if got := isDisallowedWebhookIP(ip); got != c.want {
				t.Errorf("isDisallowedWebhookIP(%s) = %v, want %v", c.ip, got, c.want)
			}
		})
	}
}

func withFakeLookup(t *testing.T, ips ...string) {
	t.Helper()
	prev := LookupIP
	var parsed []net.IP
	for _, s := range ips {
		parsed = append(parsed, net.ParseIP(s))
	}
	LookupIP = func(context.Context, string, string) ([]net.IP, error) { return parsed, nil }
	t.Cleanup(func() { LookupIP = prev })
}

func TestValidateURLRejectsPrivateHost(t *testing.T) {
	withFakeLookup(t, "169.254.169.254") // cloud metadata service
	if err := ValidateURL(context.Background(), "http://metadata.internal/latest/meta-data/"); err == nil {
		t.Error("expected an error for a host resolving to a disallowed address")
	}
}

func TestValidateURLAllowsPublicHost(t *testing.T) {
	withFakeLookup(t, "203.0.113.1")
	if err := ValidateURL(context.Background(), "https://hooks.example.com/x"); err != nil {
		t.Errorf("unexpected error for a public host: %v", err)
	}
}

func TestValidateURLRejectsNonHTTPScheme(t *testing.T) {
	withFakeLookup(t, "203.0.113.1")
	if err := ValidateURL(context.Background(), "ftp://hooks.example.com/x"); err == nil {
		t.Error("expected an error for a non-http(s) scheme")
	}
}

// TestDeliverRefusesLoopbackTarget is the Limits-I1 fix proven end to end
// through the real production path (Send -> deliver -> guardedClient): a
// real local HTTP server is started, and a webhook pointed at its genuine
// loopback address must never actually reach it, because guardedClient's
// DialContext re-validates every real connection attempt regardless of
// what ValidateURL decided at webhook-creation time.
func TestDeliverRefusesLoopbackTarget(t *testing.T) {
	// deliver runs in its own goroutine (Send is fire-and-forget); wrap
	// LookupIP so the test can wait for that goroutine to actually reach
	// (and pass through) the guard before returning. Without this, the
	// test could return while deliver's goroutine is still mid-flight,
	// racing its read of the LookupIP package var against a later test's
	// write to it (withFakeLookup) — a real data race -race would catch,
	// not just a timing nicety.
	done := make(chan struct{})
	prev := LookupIP
	LookupIP = func(ctx context.Context, network, host string) ([]net.IP, error) {
		defer close(done)
		return prev(ctx, network, host)
	}
	t.Cleanup(func() { LookupIP = prev })

	var hit atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	Send([]Endpoint{{URL: srv.URL}}, []byte(`{}`))

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the SSRF guard's lookup was never invoked")
	}
	if hit.Load() {
		t.Fatal("webhook delivery reached a loopback target; the SSRF guard must block this")
	}
}

// TestValidateDialAddrRejectsPrivateHost and
// TestValidateDialAddrAllowsPublicHostAndReturnsResolvedIP exercise the
// exact function guardedDialContext calls on EVERY real connection
// attempt — the original request and each redirect hop alike, since
// net/http re-invokes Transport.DialContext per connection regardless of
// which request triggered it. That's what closes the "worsened by
// following redirects" half of Limits-I1: there is no separate,
// once-only check a redirect could route around.
func TestValidateDialAddrRejectsPrivateHost(t *testing.T) {
	withFakeLookup(t, "10.0.0.5")
	if _, _, err := validateDialAddr(context.Background(), "internal-service:443"); err == nil {
		t.Error("expected an error for a host resolving to a private address")
	}
}

func TestValidateDialAddrAllowsPublicHostAndReturnsResolvedIP(t *testing.T) {
	withFakeLookup(t, "203.0.113.1")
	ip, port, err := validateDialAddr(context.Background(), "hooks.example.com:443")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ip != "203.0.113.1" || port != "443" {
		t.Errorf("got ip=%s port=%s, want 203.0.113.1/443", ip, port)
	}
}

// TestSendDropsWhenQueueFullWithoutBlocking is the Limits-I2 fix: Send must
// never grow goroutines/connections without bound, no matter how many
// endpoints a DLP incident fans out to. It saturates all `workers` real
// worker goroutines with a resolver that blocks until released, floods the
// shared queue well past its capacity, and asserts (a) Send itself never
// blocks the caller and (b) the excess is dropped rather than queued or
// spawned as new goroutines.
func TestSendDropsWhenQueueFullWithoutBlocking(t *testing.T) {
	block := make(chan struct{})
	var calls atomic.Int32
	prev := LookupIP
	LookupIP = func(ctx context.Context, _, _ string) ([]net.IP, error) {
		calls.Add(1)
		select {
		case <-block:
		case <-ctx.Done():
		}
		return nil, nil
	}
	t.Cleanup(func() {
		close(block)
		// Every accepted job (the `workers` that saturated real workers,
		// plus whatever fit in the buffer) must finish calling LookupIP
		// before it's safe to restore it — otherwise a straggler could
		// still be reading this package var when a later test writes to
		// it, the same race TestDeliverRefusesLoopbackTarget guards against.
		want := int32(workers + chanSize)
		deadline := time.Now().Add(2 * time.Second)
		for calls.Load() < want && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		LookupIP = prev
	})

	// Saturate all real workers first.
	for i := 0; i < workers; i++ {
		Send([]Endpoint{{URL: "http://blocked-target.test/"}}, []byte(`{}`))
	}
	deadline := time.Now().Add(2 * time.Second)
	for calls.Load() < int32(workers) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if calls.Load() < int32(workers) {
		t.Fatal("workers never became saturated")
	}

	before := Dropped()
	start := time.Now()
	for i := 0; i < chanSize+50; i++ {
		Send([]Endpoint{{URL: "http://overflow-target.test/"}}, []byte(`{}`))
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Send took %v to enqueue %d overflowing endpoints; want near-instant (non-blocking)", elapsed, chanSize+50)
	}
	if Dropped() <= before {
		t.Error("expected the dropped counter to increase once the queue filled up")
	}
}

// TestDoDeliverDrainsBodyAllowingConnectionReuse is the Limits-M1 fix
// (a second, independent occurrence of the same bug class already fixed in
// internal/dlp.ModelScan): deliver never read the response body at all,
// success or failure — left unread, it prevents Go's http.Transport from
// reusing the connection, forcing a fresh TCP connection per delivery
// instead of pooling it. doDeliver takes the client as a parameter
// specifically so this is testable against a plain client; deliver itself
// always uses the real (SSRF-guarded) client, which a loopback test target
// could never clear.
func TestDoDeliverDrainsBodyAllowingConnectionReuse(t *testing.T) {
	var conns int32
	// NewUnstartedServer + manual Start, not NewServer: NewServer starts
	// serving immediately, which races against setting Config.ConnState
	// afterward (caught by -race).
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(strings.Repeat("x", 10*1024*1024)))
	}))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			atomic.AddInt32(&conns, 1)
		}
	}
	srv.Start()
	defer srv.Close()

	hc := srv.Client()
	for i := 0; i < 3; i++ {
		req, err := http.NewRequest(http.MethodPost, srv.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := doDeliver(hc, req); err != nil {
			t.Fatalf("doDeliver: %v", err)
		}
	}

	if got := atomic.LoadInt32(&conns); got > 1 {
		t.Errorf("accepted %d TCP connections for 3 sequential deliveries on a keep-alive client, want 1 — the response body must be drained to allow connection reuse", got)
	}
}
