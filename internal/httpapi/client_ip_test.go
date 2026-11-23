package httpapi

import (
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/config"
)

func TestClientIP(t *testing.T) {
	cases := []struct {
		name, remote string
		xff          []string
		want         string
	}{
		{"direct public peer, no header", "203.0.113.7:5000", nil, "203.0.113.7"},
		{"direct public peer ignores a spoofed header", "203.0.113.7:5000", []string{"198.51.100.1"}, "203.0.113.7"},
		{"proxy without header", "10.42.0.5:4000", nil, "10.42.0.5"},
		{"single proxy appends the client", "10.42.0.5:4000", []string{"203.0.113.7"}, "203.0.113.7"},
		{"spoofed entries left of the real client are ignored", "10.42.0.5:4000", []string{"198.51.100.1, 203.0.113.7"}, "203.0.113.7"},
		{"proxy chain: skip internal hops from the right", "10.42.0.5:4000", []string{"198.51.100.1, 203.0.113.7, 10.42.1.9"}, "203.0.113.7"},
		{"multiple header lines are one list", "10.42.0.5:4000", []string{"198.51.100.1", "203.0.113.7, 10.42.1.9"}, "203.0.113.7"},
		{"LAN client behind a proxy", "127.0.0.1:4000", []string{"192.168.1.20"}, "192.168.1.20"},
		{"all internal: use the rightmost", "10.42.0.5:4000", []string{"10.0.0.1, 192.168.1.20"}, "192.168.1.20"},
		{"malformed entry discards what is left of it", "10.42.0.5:4000", []string{"198.51.100.1, junk, 192.168.1.20"}, "192.168.1.20"},
		{"ipv6 client", "[::1]:4000", []string{"2001:db8::1"}, "2001:db8::1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/auth/login", nil)
			r.RemoteAddr = tc.remote
			for _, v := range tc.xff {
				r.Header.Add("X-Forwarded-For", v)
			}
			if got := clientIP(r, config.DefaultTrustedProxyCIDRs); got != tc.want {
				t.Fatalf("clientIP = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestClientIPCustomTrustedCIDRs proves TRUSTED_PROXY_CIDRS narrows who may
// speak X-Forwarded-For: a peer outside the list is the client even when
// private, and XFF entries inside the list are skipped as proxy hops.
func TestClientIPCustomTrustedCIDRs(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("10.42.0.0/16")}
	cases := []struct {
		name, remote, xff, want string
	}{
		{"trusted peer: first untrusted entry from the right", "10.42.0.5:1", "198.51.100.1, 192.168.1.20, 10.42.3.3", "192.168.1.20"},
		{"untrusted private peer: header ignored", "192.168.1.1:1", "203.0.113.7", "192.168.1.1"},
		{"trusted peer, all hops trusted: rightmost", "10.42.0.5:1", "10.42.9.9, 10.42.3.3", "10.42.3.3"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/auth/login", nil)
			r.RemoteAddr = tc.remote
			r.Header.Set("X-Forwarded-For", tc.xff)
			if got := clientIP(r, trusted); got != tc.want {
				t.Fatalf("clientIP = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestServerClientIPUsesConfiguredCIDRs proves the handlers' helper reads the
// configured list rather than the default.
func TestServerClientIPUsesConfiguredCIDRs(t *testing.T) {
	r := httptest.NewRequest("POST", "/auth/login", nil)
	r.RemoteAddr = "10.0.0.5:1"
	r.Header.Set("X-Forwarded-For", "203.0.113.7")
	if got := (&Server{}).clientIP(r); got != "203.0.113.7" {
		t.Fatalf("default CIDRs: got %q", got)
	}
	s := &Server{cfg: &config.Config{TrustedProxyCIDRs: []netip.Prefix{netip.MustParsePrefix("10.42.0.0/16")}}}
	if got := s.clientIP(r); got != "10.0.0.5" {
		t.Fatalf("configured CIDRs: a peer outside them must be the client, got %q", got)
	}
}
