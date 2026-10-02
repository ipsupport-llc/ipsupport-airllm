package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/store"
)

// TestHandleAdminCreateWebhookSealsSecret proves Auth-I5/Admin-API-I4's fix:
// a webhook secret is sealed before it reaches Postgres, and the same
// sealer round-trips it back to the original plaintext for signing.
func TestHandleAdminCreateWebhookSealsSecret(t *testing.T) {
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
