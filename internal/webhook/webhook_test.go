package webhook

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
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
