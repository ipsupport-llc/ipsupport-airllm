// Package webhook delivers signed JSON alerts to outbound endpoints.
package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"
)

// Endpoint is one webhook destination.
type Endpoint struct {
	URL    string
	Secret string
}

// LookupIP resolves a hostname to its IP addresses. A package variable so
// tests can inject a fake resolver instead of depending on real DNS/network
// (this codebase avoids live external calls in unit tests).
var LookupIP = net.DefaultResolver.LookupIP

// isDisallowedWebhookIP reports whether ip is an internal-only address a
// webhook must never be allowed to reach: loopback, RFC1918/RFC4193
// private ranges, link-local (including the 169.254.169.254 cloud
// metadata service), multicast, or unspecified.
func isDisallowedWebhookIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified()
}

// validateDialAddr resolves host:port and rejects it if any resolved IP is
// disallowed. It returns the first allowed IP to dial, so a second,
// unchecked DNS lookup inside the real dial can't land on a different
// address than the one just validated (DNS can change between the check
// and the connect otherwise).
func validateDialAddr(ctx context.Context, addr string) (ip, port string, err error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", "", err
	}
	ips, err := LookupIP(ctx, "ip", host)
	if err != nil {
		return "", "", err
	}
	if len(ips) == 0 {
		return "", "", fmt.Errorf("no addresses found for host %q", host)
	}
	for _, candidate := range ips {
		if isDisallowedWebhookIP(candidate) {
			return "", "", fmt.Errorf("webhook target %q resolves to a disallowed address %s", host, candidate)
		}
	}
	return ips[0].String(), port, nil
}

// guardedDialContext is the Transport's DialContext: it re-validates the
// target of EVERY connection attempt, not just the webhook's configured
// URL. That includes a redirect (Go's http.Client follows one by issuing a
// new request through this same Transport, which dials again) and a
// webhook whose DNS record changed after it passed ValidateURL at creation
// time — an attacker-controlled domain that resolved to a public IP when
// the webhook was saved, then later repointed at an internal address,
// would otherwise bypass a check made only once.
func guardedDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	ip, port, err := validateDialAddr(ctx, addr)
	if err != nil {
		return nil, err
	}
	return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, net.JoinHostPort(ip, port))
}

var guardedClient = &http.Client{
	Transport: &http.Transport{DialContext: guardedDialContext},
	Timeout:   10 * time.Second,
}

// ValidateURL rejects a webhook URL that isn't http(s), or whose host
// resolves to a disallowed (internal-only) address — the same check
// guardedDialContext enforces on every actual delivery, run eagerly here
// so a bad config is rejected immediately with a clear error instead of
// silently queuing a webhook that can only ever fail (or, worse, succeed
// until its DNS record changes).
func ValidateURL(ctx context.Context, rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("URL scheme must be http or https, got %q", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return errors.New("URL is missing a host")
	}
	ips, err := LookupIP(ctx, "ip", host)
	if err != nil {
		return fmt.Errorf("resolve host %q: %w", host, err)
	}
	for _, ip := range ips {
		if isDisallowedWebhookIP(ip) {
			return fmt.Errorf("host %q resolves to a disallowed address %s", host, ip)
		}
	}
	return nil
}

// chanSize and workers bound webhook delivery's fan-out, the same way
// internal/capture.Pipeline and internal/ledger.Ledger bound theirs:
// a goroutine (and outbound connection) per endpoint, with no cap, meant
// unlimited concurrent deliveries could pile up under a sustained run of
// DLP incidents (the request path that calls Send) — unbounded goroutine
// and connection growth, not just a slow response.
const (
	chanSize = 1024
	workers  = 4
)

type deliveryJob struct {
	endpoint Endpoint
	body     []byte
}

var (
	queue        = make(chan deliveryJob, chanSize)
	dropped      atomic.Int64
	startWorkers sync.Once
)

// Dropped returns how many deliveries were dropped because the queue was
// full.
func Dropped() int64 { return dropped.Load() }

func ensureWorkers() {
	startWorkers.Do(func() {
		for i := 0; i < workers; i++ {
			go func() {
				for j := range queue {
					deliver(j.endpoint, j.body)
				}
			}()
		}
	})
}

// Send posts body to each endpoint asynchronously (fire-and-forget) through
// a fixed worker pool. When an endpoint has a secret, the body is signed
// with HMAC-SHA256 in the X-AirLLM-Signature header ("sha256=<hex>"). If
// the queue is full, a delivery is dropped (Dropped() increments and a
// warning is logged) rather than spawning another goroutine — delivery is
// already best-effort, so dropping under sustained overload is consistent
// with Send's existing fire-and-forget contract, not a new one.
func Send(endpoints []Endpoint, body []byte) {
	ensureWorkers()
	for _, e := range endpoints {
		select {
		case queue <- deliveryJob{endpoint: e, body: body}:
		default:
			dropped.Add(1)
			slog.Warn("webhook delivery dropped; queue full", "url", e.URL)
		}
	}
}

func deliver(e Endpoint, body []byte) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.URL, bytes.NewReader(body))
	if err != nil {
		slog.Error("webhook build failed", "url", e.URL, "err", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if e.Secret != "" {
		mac := hmac.New(sha256.New, []byte(e.Secret))
		mac.Write(body)
		req.Header.Set("X-AirLLM-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}

	resp, err := doDeliver(guardedClient, req)
	if err != nil {
		slog.Error("webhook delivery failed", "url", e.URL, "err", err)
		return
	}
	if resp.StatusCode/100 != 2 {
		slog.Warn("webhook non-2xx", "url", e.URL, "status", resp.StatusCode)
	}
}

// doDeliver performs req via hc, draining and closing the response body
// before returning so the transport can reuse the connection for the next
// delivery — an unread body (this endpoint's response is never otherwise
// read, success or failure) forces a fresh connection per delivery instead
// of pooling it. Takes hc as a parameter (production always passes
// guardedClient) so this logic is testable against a plain client, since a
// test webhook target would otherwise have to be a real external host to
// clear the SSRF guard.
func doDeliver(hc *http.Client, req *http.Request) (*http.Response, error) {
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp, nil
}
