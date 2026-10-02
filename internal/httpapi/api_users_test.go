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
	"github.com/ipsupport-llc/ipsupport-airllm/internal/store"
)

func TestValidateNewUser(t *testing.T) {
	known := map[string]bool{"airllm_admin": true, "airllm_user": true}
	if err := validateNewUser("alice", []string{"airllm_user"}, "longenough", known); err != nil {
		t.Errorf("valid user rejected: %v", err)
	}
	if err := validateNewUser("", []string{"airllm_user"}, "longenough", known); err == nil {
		t.Error("empty username must fail")
	}
	if err := validateNewUser("bob", []string{"airllm_user"}, "short", known); err == nil {
		t.Error("short password must fail")
	}
	if err := validateNewUser("bob", []string{"nope"}, "longenough", known); err == nil {
		t.Error("unknown role must fail")
	}
}

func TestValidatePasswordLen(t *testing.T) {
	if err := validatePassword("1234567"); err == nil {
		t.Error("7-char password must fail (<8)")
	}
	if err := validatePassword("12345678"); err != nil {
		t.Errorf("8-char password must pass: %v", err)
	}
}

// TestGuardLastAdminSerializesConcurrentGuards proves guardLastAdminTx's
// admin-row lock actually serializes two concurrent guard checks (Admin API
// I2 fix) rather than letting them race: it holds LockAdminIDs open in one
// transaction (simulating a slow in-flight demote that hasn't committed
// yet), then confirms a concurrent guardLastAdminTx call for a DIFFERENT
// admin blocks until the held transaction finishes. Without the lock,
// two such guards could each read "more than one admin remains" before
// either write commits and both proceed, leaving zero admins.
func TestGuardLastAdminSerializesConcurrentGuards(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	s := &Server{st: &store.Store{PG: pool}}
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())

	var idP, idQ string
	if err := pool.QueryRow(ctx, `
		INSERT INTO users (subject, roles) VALUES ($1, $2) RETURNING id::text`,
		"guard-test-p-"+suffix, []string{auth.AdminRole},
	).Scan(&idP); err != nil {
		t.Fatalf("seed admin p: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO users (subject, roles) VALUES ($1, $2) RETURNING id::text`,
		"guard-test-q-"+suffix, []string{auth.AdminRole},
	).Scan(&idQ); err != nil {
		t.Fatalf("seed admin q: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id IN ($1, $2)`, idP, idQ)
	})

	heldTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin held tx: %v", err)
	}
	// Safety net: an early t.Fatalf below must not leave this connection
	// checked out of the pool forever (pool.Close in t.Cleanup would hang
	// waiting for it). A second Rollback, after the explicit one on the
	// success path, is a harmless no-op.
	defer heldTx.Rollback(ctx)
	if _, err := s.users().LockAdminIDs(ctx, heldTx); err != nil {
		t.Fatalf("lock admin ids: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		tx2, err := pool.Begin(ctx)
		if err != nil {
			done <- err
			return
		}
		defer tx2.Rollback(ctx)
		done <- s.guardLastAdminTx(ctx, tx2, idQ, nil, true)
	}()

	select {
	case err := <-done:
		t.Fatalf("concurrent guard completed (err=%v) before the holding transaction released its lock — admin rows aren't serialized", err)
	case <-time.After(150 * time.Millisecond):
	}

	if err := heldTx.Rollback(ctx); err != nil {
		t.Fatalf("release held tx: %v", err)
	}

	// Whatever it decides (allowed, or correctly blocked because these two
	// seeded users happen to be the only admins in this DB) is a legitimate
	// verdict once it has a fresh, post-lock-release read; what matters here
	// is only that it DOES complete instead of timing out.
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("concurrent guard never completed after the lock was released")
	}
}

// TestUpdateUserOmittedFieldsKeepStoredValue proves a PUT that only means to
// touch one field does not silently wipe roles or re-enable a disabled
// account just because the request left them out (Admin API I3 fix): the
// admin console's own form always sends every field, but this is a general
// admin API, and any other caller omitting roles/disabled must get
// "keep what's stored", not "reset to the zero value".
func TestUpdateUserOmittedFieldsKeepStoredValue(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	s := &Server{st: &store.Store{PG: pool}}
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())

	var id string
	if err := pool.QueryRow(ctx, `
		INSERT INTO users (subject, email, roles, disabled) VALUES ($1, $2, $3, true) RETURNING id::text`,
		"i3-test-"+suffix, "old@example.com", []string{auth.UserRole},
	).Scan(&id); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id)
	})

	// Only email is set; roles and disabled are omitted entirely. roles is
	// included unchanged here specifically to isolate the "disabled" half
	// of the bug from the unrelated NOT-NULL crash a nil roles slice would
	// otherwise hit on the pre-fix code (a real, separate rough edge, not
	// this finding's "silently re-enables" symptom).
	body := `{"email":"new@example.com","roles":["` + auth.UserRole + `"]}`
	req := httptest.NewRequest(http.MethodPut, "/api/admin/users/"+id, strings.NewReader(body))
	req.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	s.handleUpdateUser(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("update failed: %d %s", rec.Code, rec.Body.String())
	}

	var gotEmail string
	var gotRoles []string
	var gotDisabled bool
	if err := pool.QueryRow(ctx, `SELECT email, roles, disabled FROM users WHERE id = $1`, id).
		Scan(&gotEmail, &gotRoles, &gotDisabled); err != nil {
		t.Fatalf("read back user: %v", err)
	}
	if gotEmail != "new@example.com" {
		t.Errorf("email = %q, want new@example.com", gotEmail)
	}
	if len(gotRoles) != 1 || gotRoles[0] != auth.UserRole {
		t.Errorf("roles = %v, want unchanged [%s] — omitted roles must not be wiped", gotRoles, auth.UserRole)
	}
	if !gotDisabled {
		t.Error("disabled = false, want unchanged true — omitting disabled must not silently re-enable a disabled account")
	}
}

// TestUpdateUserExplicitFieldsStillApply proves the fix doesn't just ignore
// roles/disabled — an explicitly supplied value (including the zero value,
// false) still applies.
func TestUpdateUserExplicitFieldsStillApply(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	s := &Server{st: &store.Store{PG: pool}}
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())

	var id string
	if err := pool.QueryRow(ctx, `
		INSERT INTO users (subject, roles, disabled) VALUES ($1, $2, false) RETURNING id::text`,
		"i3-explicit-test-"+suffix, []string{auth.UserRole},
	).Scan(&id); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id)
	})

	reqBody, _ := json.Marshal(map[string]any{"roles": []string{}, "disabled": true})
	req := httptest.NewRequest(http.MethodPut, "/api/admin/users/"+id, strings.NewReader(string(reqBody)))
	req.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	s.handleUpdateUser(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("update failed: %d %s", rec.Code, rec.Body.String())
	}

	var gotRoles []string
	var gotDisabled bool
	if err := pool.QueryRow(ctx, `SELECT roles, disabled FROM users WHERE id = $1`, id).
		Scan(&gotRoles, &gotDisabled); err != nil {
		t.Fatalf("read back user: %v", err)
	}
	if len(gotRoles) != 0 {
		t.Errorf("roles = %v, want explicitly cleared to empty", gotRoles)
	}
	if !gotDisabled {
		t.Error("disabled = false, want explicitly set to true")
	}
}
