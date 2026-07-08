package seed

import (
	"testing"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/auth"
)

func TestBuiltinRolesPolicy(t *testing.T) {
	roles := BuiltinRoles()
	byName := map[string]BuiltinRole{}
	for _, r := range roles {
		byName[r.Role] = r
	}

	admin, ok := byName[auth.AdminRole]
	if !ok {
		t.Fatalf("BuiltinRoles missing %q", auth.AdminRole)
	}
	if len(admin.AllowedModels) != 1 || admin.AllowedModels[0] != "*" {
		t.Errorf("admin AllowedModels = %v, want [*]", admin.AllowedModels)
	}
	if !admin.AllowPassthrough {
		t.Error("admin AllowPassthrough must be true")
	}

	auditor, ok := byName[auth.AuditorRole]
	if !ok {
		t.Fatalf("BuiltinRoles missing %q", auth.AuditorRole)
	}
	if len(auditor.AllowedModels) != 0 {
		t.Errorf("auditor AllowedModels = %v, want empty", auditor.AllowedModels)
	}
	if auditor.AllowPassthrough {
		t.Error("auditor AllowPassthrough must be false")
	}

	if len(roles) != 2 {
		t.Errorf("BuiltinRoles() has %d entries, want exactly 2 (demo roles stay dev-only)", len(roles))
	}
}
