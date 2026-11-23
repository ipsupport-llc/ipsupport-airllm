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

func putWebhook(s *Server, id string, body map[string]any) *httptest.ResponseRecorder {
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPut, "/api/admin/webhooks/"+id, bytes.NewReader(raw))
	req.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	s.handleAdminUpdateWebhook(rec, req)
	return rec
}

// The update path validates like create, before touching the database.
func TestHandleAdminUpdateWebhookValidatesURL(t *testing.T) {
	prev := webhook.LookupIP
	webhook.LookupIP = func(context.Context, string, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("169.254.169.254")}, nil
	}
	t.Cleanup(func() { webhook.LookupIP = prev })
	s := &Server{auditHook: func(context.Context, string, string, string, any) {}}

	if rec := putWebhook(s, "00000000-0000-0000-0000-000000000001", map[string]any{"url": ""}); rec.Code != http.StatusBadRequest {
		t.Errorf("empty url: got %d, want 400", rec.Code)
	}
	rec := putWebhook(s, "00000000-0000-0000-0000-000000000001", map[string]any{"url": "http://metadata.internal/latest/"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("internal-resolving url: got %d, want 400 — %s", rec.Code, rec.Body.String())
	}
}

// TestHandleAdminUpdateWebhookKeepsOrReplacesSecret: an update without a
// secret keeps the sealed one; with a secret it is resealed and replaced.
func TestHandleAdminUpdateWebhookKeepsOrReplacesSecret(t *testing.T) {
	fakePublicDNS(t)
	pool := testPool(t)
	ctx := context.Background()
	var audited []string
	s := &Server{
		st:     &store.Store{PG: pool},
		sealer: testAuditSealer(t),
		auditHook: func(_ context.Context, _, action, _ string, _ any) {
			audited = append(audited, action)
		},
	}

	name := fmt.Sprintf("webhook-test-update-%d", time.Now().UnixNano())
	raw, _ := json.Marshal(map[string]any{"name": name, "url": "https://example.test/hook", "secret": "first", "enabled": true})
	req := httptest.NewRequest(http.MethodPost, "/api/admin/webhooks", bytes.NewReader(raw))
	rec := httptest.NewRecorder()
	s.handleAdminCreateWebhook(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	t.Cleanup(func() { pool.Exec(context.Background(), `DELETE FROM webhooks WHERE id = $1`, created.ID) })

	read := func() (url string, events []string, enabled bool, secretEnc []byte) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT url, events, enabled, secret_enc FROM webhooks WHERE id = $1`, created.ID).
			Scan(&url, &events, &enabled, &secretEnc); err != nil {
			t.Fatalf("read webhook: %v", err)
		}
		return
	}
	_, _, _, before := read()

	rec = putWebhook(s, created.ID, map[string]any{
		"name": name, "url": "https://example.test/other", "events": []string{"dlp.incident", "dlp.false_negative"}, "enabled": false,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("update without secret: %d %s", rec.Code, rec.Body.String())
	}
	url, events, enabled, after := read()
	if url != "https://example.test/other" || len(events) != 2 || enabled {
		t.Errorf("fields not updated: url=%q events=%v enabled=%v", url, events, enabled)
	}
	if !bytes.Equal(before, after) {
		t.Error("an update without a secret changed the stored secret")
	}

	if rec := putWebhook(s, created.ID, map[string]any{"name": name, "url": url, "secret": "second", "enabled": true}); rec.Code != http.StatusOK {
		t.Fatalf("update with secret: %d %s", rec.Code, rec.Body.String())
	}
	_, _, _, replaced := read()
	if got := s.openWebhookSecret(replaced); got != "second" {
		t.Errorf("secret after update = %q, want \"second\"", got)
	}
	if len(audited) < 3 || audited[len(audited)-1] != "webhook.update" {
		t.Errorf("audit actions = %v, want webhook.update logged", audited)
	}
}

func TestHandleAdminUpdateWebhookUnknownID(t *testing.T) {
	fakePublicDNS(t)
	pool := testPool(t)
	s := &Server{st: &store.Store{PG: pool}, sealer: testAuditSealer(t), auditHook: func(context.Context, string, string, string, any) {}}
	rec := putWebhook(s, "00000000-0000-0000-0000-0000000000ff", map[string]any{"url": "https://example.test/hook"})
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown id: got %d, want 404 — %s", rec.Code, rec.Body.String())
	}
}
