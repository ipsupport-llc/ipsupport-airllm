package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/auth"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/store"
)

// TestRequireSessionRejectsStaleCookie proves a disabled or demoted user's
// existing session cookie stops granting its original access on the very
// next request — the bug this guards against: requireSession used to trust
// the cookie's baked-in roles for the whole 12h TTL and never checked
// users.disabled at all, so disabling or demoting a user had no effect
// until the cookie expired or the user logged in again.
func TestRequireSessionRejectsStaleCookie(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	sess := auth.NewSession([]byte("test-signing-key"))
	s := &Server{st: &store.Store{PG: pool}, auth: sess}

	subject := fmt.Sprintf("stale-session-test-%d", time.Now().UnixNano())

	var userID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO users (subject, roles) VALUES ($1, $2) RETURNING id::text`,
		subject, []string{auth.AdminRole},
	).Scan(&userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, userID); err != nil {
			t.Errorf("cleanup user: %v", err)
		}
	})

	// One cookie, issued once, baked in as admin — never reissued for the
	// rest of the test, so any change in behavior below comes from the
	// server re-checking the DB, not from a fresh login.
	rec := httptest.NewRecorder()
	sess.SetSession(rec, httptest.NewRequest(http.MethodGet, "/", nil), auth.Principal{Subject: subject, Roles: []string{auth.AdminRole}})
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == "air_session" {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("no session cookie set")
	}

	var gotAdmin bool
	handler := s.requireSession(func(w http.ResponseWriter, r *http.Request) {
		sv, _ := sessionFrom(r.Context())
		gotAdmin = sv.principal.IsAdmin()
		w.WriteHeader(http.StatusOK)
	})
	do := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.AddCookie(cookie)
		w := httptest.NewRecorder()
		handler(w, req)
		return w
	}

	if w := do(); w.Code != http.StatusOK || !gotAdmin {
		t.Fatalf("expected OK + admin for a fresh active admin, got code=%d admin=%v", w.Code, gotAdmin)
	}

	if _, err := pool.Exec(ctx, `UPDATE users SET disabled = true WHERE id = $1`, userID); err != nil {
		t.Fatalf("disable user: %v", err)
	}
	if w := do(); w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for a disabled user's still-valid cookie, got %d", w.Code)
	}

	if _, err := pool.Exec(ctx, `UPDATE users SET disabled = false, roles = '{}' WHERE id = $1`, userID); err != nil {
		t.Fatalf("re-enable + demote user: %v", err)
	}
	if w := do(); w.Code != http.StatusOK || gotAdmin {
		t.Fatalf("expected OK but non-admin for a demoted user's stale admin cookie, got code=%d admin=%v", w.Code, gotAdmin)
	}
}
