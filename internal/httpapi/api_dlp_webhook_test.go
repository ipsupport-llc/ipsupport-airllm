package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/store"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/webhook"
)

// fakePublicDNS swaps webhook.LookupIP for the test's duration so a webhook
// URL's host resolves to a fixed, non-internal IP without depending on real
// DNS — these tests only exercise handleAdminCreateWebhook, never an actual
// delivery, so the resolved address just needs to pass the SSRF guard
// (Limits-I1 fix).
func fakePublicDNS(t *testing.T) {
	t.Helper()
	prev := webhook.LookupIP
	webhook.LookupIP = func(context.Context, string, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("203.0.113.1")}, nil
	}
	t.Cleanup(func() { webhook.LookupIP = prev })
}

// TestHandleAdminCreateWebhookSealsSecret proves Auth-I5/Admin-API-I4's fix:
// a webhook secret is sealed before it reaches Postgres, and the same
// sealer round-trips it back to the original plaintext for signing.
func TestHandleAdminCreateWebhookSealsSecret(t *testing.T) {
	fakePublicDNS(t)
	pool := testPool(t)
	ctx := context.Background()
	sl := testAuditSealer(t)
	s := &Server{
		st:        &store.Store{PG: pool},
		sealer:    sl,
		auditHook: func(context.Context, string, string, string, any) {},
	}

	const secret = "whsec_super_secret_value"
	name := fmt.Sprintf("webhook-test-%d", time.Now().UnixNano())
	body, _ := json.Marshal(map[string]any{
		"name": name, "url": "https://example.test/hook", "secret": secret, "enabled": true,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/admin/webhooks", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleAdminCreateWebhook(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create webhook: %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), `DELETE FROM webhooks WHERE id = $1`, created.ID)
	})

	var stored []byte
	if err := pool.QueryRow(ctx, `SELECT secret_enc FROM webhooks WHERE id = $1`, created.ID).Scan(&stored); err != nil {
		t.Fatalf("read secret_enc: %v", err)
	}
	if stored == nil {
		t.Fatal("secret_enc is NULL, want a sealed value")
	}
	if bytes.Contains(stored, []byte(secret)) {
		t.Fatal("plaintext secret leaked into the stored ciphertext")
	}

	eps, err := s.st.WebhooksForEvent(ctx, "dlp.incident")
	if err != nil {
		t.Fatalf("WebhooksForEvent: %v", err)
	}
	var found bool
	for _, e := range eps {
		if e.ID != created.ID {
			continue
		}
		found = true
		if got := s.openWebhookSecret(e.SecretEnc); got != secret {
			t.Errorf("openWebhookSecret round-trip = %q, want %q", got, secret)
		}
	}
	if !found {
		t.Fatal("created webhook not returned by WebhooksForEvent (check default events/enabled)")
	}
}

// TestHandleAdminCreateWebhookNoSecretStaysNull covers a webhook created
// without a secret: secret_enc must stay NULL (not a sealed empty string),
// and openWebhookSecret must return "" for it (unsigned delivery).
func TestHandleAdminCreateWebhookNoSecretStaysNull(t *testing.T) {
	fakePublicDNS(t)
	pool := testPool(t)
	ctx := context.Background()
	sl := testAuditSealer(t)
	s := &Server{
		st:        &store.Store{PG: pool},
		sealer:    sl,
		auditHook: func(context.Context, string, string, string, any) {},
	}

	name := fmt.Sprintf("webhook-test-nosecret-%d", time.Now().UnixNano())
	body, _ := json.Marshal(map[string]any{"name": name, "url": "https://example.test/hook"})
	req := httptest.NewRequest(http.MethodPost, "/api/admin/webhooks", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleAdminCreateWebhook(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create webhook: %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	json.Unmarshal(rec.Body.Bytes(), &created)
	t.Cleanup(func() {
		pool.Exec(context.Background(), `DELETE FROM webhooks WHERE id = $1`, created.ID)
	})

	var stored []byte
	if err := pool.QueryRow(ctx, `SELECT secret_enc FROM webhooks WHERE id = $1`, created.ID).Scan(&stored); err != nil {
		t.Fatalf("read secret_enc: %v", err)
	}
	if stored != nil {
		t.Errorf("secret_enc = %v, want NULL when no secret was supplied", stored)
	}
	if got := s.openWebhookSecret(stored); got != "" {
		t.Errorf("openWebhookSecret(nil) = %q, want \"\"", got)
	}
}

// TestHandleAdminCreateWebhookRejectsInternalURL is the Limits-I1 fix: a
// webhook URL whose host resolves to an internal-only address (here, the
// cloud metadata service) must be rejected at creation time with a clear
// 400, not silently accepted and left to fail (or succeed) at delivery
// time.
func TestHandleAdminCreateWebhookRejectsInternalURL(t *testing.T) {
	prev := webhook.LookupIP
	webhook.LookupIP = func(context.Context, string, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("169.254.169.254")}, nil
	}
	t.Cleanup(func() { webhook.LookupIP = prev })

	pool := testPool(t)
	sl := testAuditSealer(t)
	s := &Server{
		st:        &store.Store{PG: pool},
		sealer:    sl,
		auditHook: func(context.Context, string, string, string, any) {},
	}

	body, _ := json.Marshal(map[string]any{
		"name": "ssrf-test", "url": "http://metadata.internal/latest/meta-data/", "enabled": true,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/admin/webhooks", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleAdminCreateWebhook(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("create webhook with an internal-resolving URL: got %d, want 400 — body: %s", rec.Code, rec.Body.String())
	}

	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM webhooks WHERE name = 'ssrf-test'`).Scan(&n); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if n != 0 {
		t.Errorf("rejected webhook was persisted anyway (%d rows)", n)
		pool.Exec(context.Background(), `DELETE FROM webhooks WHERE name = 'ssrf-test'`)
	}
}
