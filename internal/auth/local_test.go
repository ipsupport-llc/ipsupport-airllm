package auth

import (
	"context"
	"testing"
)

type fakeUsers struct {
	byName  map[string]UserRow
	admins  int
	created []UserRow
}

func (f *fakeUsers) ByUsername(_ context.Context, u string) (UserRow, bool, error) {
	r, ok := f.byName[u]
	return r, ok, nil
}
func (f *fakeUsers) CountAdmins(_ context.Context) (int, error) { return f.admins, nil }
func (f *fakeUsers) CreateLocal(_ context.Context, u UserRow) (string, error) {
	f.created = append(f.created, u)
	return "new-id", nil
}

func newLocal(t *testing.T, users map[string]UserRow) *LocalAuth {
	t.Helper()
	return NewLocalAuth(&fakeUsers{byName: users}, NewSession([]byte("0123456789abcdef0123456789abcdef")))
}

func TestLocalLoginSuccess(t *testing.T) {
	h, _ := HashPassword("pw12345678")
	la := newLocal(t, map[string]UserRow{"admin": {Subject: "admin", Roles: []string{AdminRole}, PasswordHash: h}})
	p, ok := la.Login("admin", "pw12345678")
	if !ok || !p.IsAdmin() {
		t.Fatalf("admin login should succeed with admin role, ok=%v p=%+v", ok, p)
	}
}

func TestLocalLoginRejectsDisabledWrongUnknown(t *testing.T) {
	h, _ := HashPassword("pw12345678")
	la := newLocal(t, map[string]UserRow{
		"admin": {Subject: "admin", PasswordHash: h},
		"off":   {Subject: "off", PasswordHash: h, Disabled: true},
	})
	if _, ok := la.Login("admin", "wrong"); ok {
		t.Error("wrong password must fail")
	}
	if _, ok := la.Login("off", "pw12345678"); ok {
		t.Error("disabled user must fail")
	}
	if _, ok := la.Login("ghost", "pw12345678"); ok {
		t.Error("unknown user must fail")
	}
}

func TestEnsureBootstrapAdmin(t *testing.T) {
	// no admins -> create with env password, not logged/returned
	f := &fakeUsers{byName: map[string]UserRow{}, admins: 0}
	created, gen, err := EnsureBootstrapAdmin(context.Background(), f, "admin", "envsecret")
	if err != nil || !created || gen != "" || len(f.created) != 1 {
		t.Fatalf("env bootstrap: created=%v gen=%q err=%v n=%d", created, gen, err, len(f.created))
	}
	if !CheckPassword(f.created[0].PasswordHash, "envsecret") {
		t.Error("bootstrap admin must use the env password")
	}
	// admins already exist -> no-op
	f2 := &fakeUsers{admins: 1}
	created2, _, _ := EnsureBootstrapAdmin(context.Background(), f2, "admin", "")
	if created2 || len(f2.created) != 0 {
		t.Error("existing admin must skip bootstrap")
	}
	// no env password -> generate and return it (caller logs once)
	f3 := &fakeUsers{byName: map[string]UserRow{}}
	_, gen3, _ := EnsureBootstrapAdmin(context.Background(), f3, "admin", "")
	if gen3 == "" || !CheckPassword(f3.created[0].PasswordHash, gen3) {
		t.Error("generated bootstrap password must be returned and match the hash")
	}
}
