package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPrincipalRoles(t *testing.T) {
	admin := Principal{Roles: []string{AdminRole}}
	if !admin.IsAdmin() {
		t.Error("admin role should be admin")
	}
	user := Principal{Roles: []string{UserRole}}
	if user.IsAdmin() {
		t.Error("user role should not be admin")
	}
	if !user.HasRole(UserRole) || user.HasRole("nope") {
		t.Error("HasRole wrong")
	}
}

func adminPassword(creds []Credential) string {
	for _, c := range creds {
		if c.Username == "admin" {
			return c.Password
		}
	}
	return ""
}

func TestMockLogin(t *testing.T) {
	m, creds := NewMock()
	pw := adminPassword(creds)
	if pw == "" {
		t.Fatal("no admin credential generated")
	}
	if _, ok := m.Login("admin", "wrong"); ok {
		t.Error("wrong password accepted")
	}
	if _, ok := m.Login("ghost", pw); ok {
		t.Error("unknown user accepted")
	}
	p, ok := m.Login("admin", pw)
	if !ok || !p.IsAdmin() {
		t.Fatalf("admin login failed: ok=%v admin=%v", ok, p.IsAdmin())
	}
}

func TestMockSessionRoundTrip(t *testing.T) {
	m, creds := NewMock()
	p, _ := m.Login("admin", adminPassword(creds))

	rec := httptest.NewRecorder()
	m.SetSession(rec, p)
	cookies := rec.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("no session cookie set")
	}

	req := httptest.NewRequest("GET", "/", nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	got, err := m.Authenticate(req)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if got.Subject != "admin" || !got.IsAdmin() {
		t.Errorf("round-trip principal wrong: %+v", got)
	}
}

func TestIsAuditor(t *testing.T) {
	admin := Principal{Roles: []string{AdminRole}}
	if !admin.IsAuditor() {
		t.Error("admin must pass IsAuditor")
	}
	auditor := Principal{Roles: []string{AuditorRole}}
	if !auditor.IsAuditor() {
		t.Error("auditor must pass IsAuditor")
	}
	user := Principal{Roles: []string{UserRole}}
	if user.IsAuditor() {
		t.Error("plain user must fail IsAuditor")
	}
	none := Principal{}
	if none.IsAuditor() {
		t.Error("no-role principal must fail IsAuditor")
	}
}

func TestMockAuthenticateRejects(t *testing.T) {
	m, _ := NewMock()

	// No cookie.
	if _, err := m.Authenticate(httptest.NewRequest("GET", "/", nil)); err == nil {
		t.Error("expected error with no cookie")
	}

	// Tampered cookie.
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: "garbage.signature"})
	if _, err := m.Authenticate(req); err == nil {
		t.Error("expected error with tampered cookie")
	}
}
