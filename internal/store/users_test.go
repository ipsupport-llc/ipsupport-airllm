package store

import (
	"context"
	"errors"
	"testing"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/auth"
)

func TestUpsertOIDCCreatesAndRefreshesUser(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	p := &PGUsers{st: &Store{PG: pool}}
	const subject = "oidctest-user"
	t.Cleanup(func() {
		pool.Exec(context.Background(), `DELETE FROM users WHERE subject = $1`, subject)
	})

	id, err := p.UpsertOIDC(ctx, auth.Principal{Subject: subject, Email: "a@test", Roles: []string{"airllm_user"}})
	if err != nil {
		t.Fatalf("UpsertOIDC (create): %v", err)
	}

	id2, err := p.UpsertOIDC(ctx, auth.Principal{Subject: subject, Email: "b@test", Roles: []string{"airllm_admin"}})
	if err != nil {
		t.Fatalf("UpsertOIDC (refresh): %v", err)
	}
	if id2 != id {
		t.Errorf("refresh got a different id: %s != %s", id2, id)
	}

	u, ok, err := p.ByUsername(ctx, subject)
	if err != nil || !ok {
		t.Fatalf("ByUsername: ok=%v err=%v", ok, err)
	}
	if u.Email != "b@test" {
		t.Errorf("email = %q, want refreshed value b@test", u.Email)
	}
	if len(u.Roles) != 1 || u.Roles[0] != "airllm_admin" {
		t.Errorf("roles = %v, want refreshed [airllm_admin]", u.Roles)
	}
	if u.AuthSource != "oidc" {
		t.Errorf("auth_source = %q, want oidc", u.AuthSource)
	}
}

func TestUpsertOIDCRefusesLocalUserSubjectConflict(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	p := &PGUsers{st: &Store{PG: pool}}
	const subject = "oidctest-local-victim"
	t.Cleanup(func() {
		pool.Exec(context.Background(), `DELETE FROM users WHERE subject = $1`, subject)
	})

	if _, err := pool.Exec(ctx, `
		INSERT INTO users (subject, email, display, roles, password_hash, password_set_at, disabled, auth_source)
		VALUES ($1, 'victim@local', 'victim', ARRAY['airllm_admin'], 'x', now(), false, 'local')`,
		subject); err != nil {
		t.Fatalf("seed local user: %v", err)
	}

	_, err := p.UpsertOIDC(ctx, auth.Principal{Subject: subject, Email: "attacker@idp", Roles: []string{"airllm_admin"}})
	if !errors.Is(err, auth.ErrLocalUserSubjectConflict) {
		t.Fatalf("UpsertOIDC error = %v, want ErrLocalUserSubjectConflict", err)
	}

	u, ok, err := p.ByUsername(ctx, subject)
	if err != nil || !ok {
		t.Fatalf("ByUsername: ok=%v err=%v", ok, err)
	}
	if u.Email != "victim@local" {
		t.Errorf("local user's email was overwritten: %q", u.Email)
	}
	if u.AuthSource != "local" {
		t.Errorf("local user's auth_source was overwritten: %q", u.AuthSource)
	}
	if len(u.Roles) != 1 || u.Roles[0] != "airllm_admin" {
		t.Errorf("local user's roles changed unexpectedly: %v", u.Roles)
	}
}
