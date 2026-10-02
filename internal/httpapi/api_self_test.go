package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/auth"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/config"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/store"
)

func TestHandleCreateKeyAuditsAction(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	subject := fmt.Sprintf("self-test-create-%d", time.Now().UnixNano())
	var userID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO users (subject, email, display, roles) VALUES ($1, $1, $1, ARRAY[]::text[])
		RETURNING id::text`, subject).Scan(&userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), `DELETE FROM api_keys WHERE user_id = $1`, userID)
		pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, userID)
	})

	var audited []string
	s := &Server{
		st:  &store.Store{PG: pool},
		cfg: &config.Config{Env: "dev"},
		auditHook: func(_ context.Context, actor, action, target string, _ any) {
			audited = append(audited, actor+":"+action+":"+target)
		},
	}

	body := strings.NewReader(`{"name":"my-key"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/keys", body)
	sessCtx := context.WithValue(req.Context(), sessCtxKey, session{
		principal: auth.Principal{Subject: subject}, userID: userID,
	})
	req = req.WithContext(sessCtx)
	rec := httptest.NewRecorder()
	s.handleCreateKey(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create key: %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if len(audited) != 1 {
		t.Fatalf("audited = %v, want exactly one key.create entry", audited)
	}
	want := subject + ":key.create:" + created.ID
	if audited[0] != want {
		t.Errorf("audited[0] = %q, want %q", audited[0], want)
	}
}

func TestHandleRevokeKeyAuditsAction(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	subject := fmt.Sprintf("self-test-revoke-%d", time.Now().UnixNano())
	var userID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO users (subject, email, display, roles) VALUES ($1, $1, $1, ARRAY[]::text[])
		RETURNING id::text`, subject).Scan(&userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	var keyID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO api_keys (user_id, name, hash, prefix, last4, policy_snapshot, status)
		VALUES ($1, 'k', 'self-test-hash', 'air_', '0001', '{}', 'active')
		RETURNING id::text`, userID).Scan(&keyID); err != nil {
		t.Fatalf("seed key: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), `DELETE FROM api_keys WHERE user_id = $1`, userID)
		pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, userID)
	})

	var audited []string
	s := &Server{
		st:  &store.Store{PG: pool},
		cfg: &config.Config{Env: "dev"},
		auditHook: func(_ context.Context, actor, action, target string, _ any) {
			audited = append(audited, actor+":"+action+":"+target)
		},
	}

	req := httptest.NewRequest(http.MethodPost, "/api/keys/"+keyID+"/revoke", nil)
	req.SetPathValue("id", keyID)
	sessCtx := context.WithValue(req.Context(), sessCtxKey, session{
		principal: auth.Principal{Subject: subject}, userID: userID,
	})
	req = req.WithContext(sessCtx)
	rec := httptest.NewRecorder()
	s.handleRevokeKey(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke key: %d %s", rec.Code, rec.Body.String())
	}

	want := []string{subject + ":key.revoke:" + keyID}
	if len(audited) != 1 || audited[0] != want[0] {
		t.Errorf("audited = %v, want %v", audited, want)
	}
}

// TestHandleRevokeKeyWritesRealAuditLogRow leaves auditHook unset so s.audit
// takes its real path (an INSERT into audit_log) instead of a test hook —
// the closest this test suite gets to a live end-to-end check without a
// real logged-in HTTP session.
func TestHandleRevokeKeyWritesRealAuditLogRow(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	subject := fmt.Sprintf("self-test-real-audit-%d", time.Now().UnixNano())
	var userID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO users (subject, email, display, roles) VALUES ($1, $1, $1, ARRAY[]::text[])
		RETURNING id::text`, subject).Scan(&userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	var keyID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO api_keys (user_id, name, hash, prefix, last4, policy_snapshot, status)
		VALUES ($1, 'k', 'self-test-hash-2', 'air_', '0002', '{}', 'active')
		RETURNING id::text`, userID).Scan(&keyID); err != nil {
		t.Fatalf("seed key: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), `DELETE FROM audit_log WHERE actor = $1`, subject)
		pool.Exec(context.Background(), `DELETE FROM api_keys WHERE user_id = $1`, userID)
		pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, userID)
	})

	s := &Server{st: &store.Store{PG: pool}, cfg: &config.Config{Env: "dev"}}

	req := httptest.NewRequest(http.MethodPost, "/api/keys/"+keyID+"/revoke", nil)
	req.SetPathValue("id", keyID)
	sessCtx := context.WithValue(req.Context(), sessCtxKey, session{
		principal: auth.Principal{Subject: subject}, userID: userID,
	})
	req = req.WithContext(sessCtx)
	rec := httptest.NewRecorder()
	s.handleRevokeKey(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke key: %d %s", rec.Code, rec.Body.String())
	}

	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_log WHERE actor = $1 AND action = 'key.revoke' AND target = $2`,
		subject, keyID).Scan(&n); err != nil {
		t.Fatalf("count audit_log rows: %v", err)
	}
	if n != 1 {
		t.Errorf("audit_log rows for this revoke = %d, want 1", n)
	}
}
