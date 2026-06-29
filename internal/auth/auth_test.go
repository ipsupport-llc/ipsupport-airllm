package auth

import "testing"

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
