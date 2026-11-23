package httpapi

import (
	"net/http/httptest"
	"testing"
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
			if got := clientIP(r); got != tc.want {
				t.Fatalf("clientIP = %q, want %q", got, tc.want)
			}
		})
	}
}
