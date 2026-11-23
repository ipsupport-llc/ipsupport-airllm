package httpapi

import (
	"net"
	"net/http"
	"net/netip"
	"strings"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/config"
)

// clientIP returns the address of the client that sent r, for the login
// lockout key and the live view, using the configured trusted proxy CIDRs.
func (s *Server) clientIP(r *http.Request) string {
	trusted := config.DefaultTrustedProxyCIDRs
	if s.cfg != nil && s.cfg.TrustedProxyCIDRs != nil {
		trusted = s.cfg.TrustedProxyCIDRs
	}
	return clientIP(r, trusted)
}

// clientIP works out the client's address. It must not be spoofable, or a
// guesser could rotate a header to dodge the login lockout:
//
//   - A peer outside the trusted CIDRs talked to us directly; X-Forwarded-For
//     is the client's own claim and is ignored.
//   - A peer inside them is one of our reverse proxies (Caddy, Traefik,
//     cloudflared). Proxies APPEND the address they saw, so the client can
//     only control entries to the left. Walking from the right, the first
//     entry outside the trusted CIDRs is the client; if every entry is
//     trusted (a client on the proxies' own network), the rightmost entry,
//     which the nearest proxy appended, is used.
func clientIP(r *http.Request, trusted []netip.Prefix) string {
	peer := r.RemoteAddr
	if h, _, err := net.SplitHostPort(peer); err == nil {
		peer = h
	}
	peerAddr, err := netip.ParseAddr(peer)
	if err != nil || !inPrefixes(peerAddr, trusted) {
		return peer
	}
	var hops []netip.Addr
	for _, line := range r.Header.Values("X-Forwarded-For") {
		for _, part := range strings.Split(line, ",") {
			a, err := netip.ParseAddr(strings.TrimSpace(part))
			if err != nil {
				// A malformed entry can't be trusted, and nothing to its
				// left can be either.
				hops = hops[:0]
				continue
			}
			hops = append(hops, a.Unmap())
		}
	}
	if len(hops) == 0 {
		return peer
	}
	for i := len(hops) - 1; i >= 0; i-- {
		if !inPrefixes(hops[i], trusted) {
			return hops[i].String()
		}
	}
	return hops[len(hops)-1].String()
}

// inPrefixes reports whether a falls inside any of prefixes.
func inPrefixes(a netip.Addr, prefixes []netip.Prefix) bool {
	a = a.Unmap()
	for _, p := range prefixes {
		if p.Contains(a) {
			return true
		}
	}
	return false
}
