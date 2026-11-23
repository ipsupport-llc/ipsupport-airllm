package httpapi

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// clientIP returns the address of the client that sent r, for the login
// lockout key. It must not be spoofable, or a guesser could rotate a header
// to dodge the lockout:
//
//   - A peer with a public address talked to us directly; X-Forwarded-For is
//     the client's own claim and is ignored.
//   - A peer with a private/loopback address is a reverse proxy (Caddy,
//     Traefik, cloudflared — every deployment shape this project ships).
//     Proxies APPEND the address they saw, so the client can only control
//     entries to the left. The rightmost public entry is the first address
//     outside our own network; if every entry is private (a LAN client), the
//     rightmost entry, which the nearest proxy appended, is used.
func clientIP(r *http.Request) string {
	peer := r.RemoteAddr
	if h, _, err := net.SplitHostPort(peer); err == nil {
		peer = h
	}
	peerAddr, err := netip.ParseAddr(peer)
	if err != nil || !isInternalAddr(peerAddr) {
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
		if !isInternalAddr(hops[i]) {
			return hops[i].String()
		}
	}
	return hops[len(hops)-1].String()
}

// isInternalAddr reports whether a is a loopback, private, or link-local
// address — one of our own proxies rather than an internet client.
func isInternalAddr(a netip.Addr) bool {
	a = a.Unmap()
	return a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast()
}
