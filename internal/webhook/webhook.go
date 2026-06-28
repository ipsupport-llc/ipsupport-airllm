// Package webhook delivers signed JSON alerts to outbound endpoints.
package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"
	"time"
)

// Endpoint is one webhook destination.
type Endpoint struct {
	URL    string
	Secret string
}

// Send posts body to each endpoint asynchronously (fire-and-forget). When an
// endpoint has a secret, the body is signed with HMAC-SHA256 in the
// X-AirLLM-Signature header ("sha256=<hex>").
func Send(endpoints []Endpoint, body []byte) {
	for _, e := range endpoints {
		e := e
		go deliver(e, body)
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

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		slog.Error("webhook delivery failed", "url", e.URL, "err", err)
		return
	}
	_ = resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		slog.Warn("webhook non-2xx", "url", e.URL, "status", resp.StatusCode)
	}
}
