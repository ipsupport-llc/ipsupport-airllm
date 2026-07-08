package seed

import (
	"context"
	"fmt"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/auth"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/store"
)

// BuiltinRole is a role policy the gateway guarantees to exist.
type BuiltinRole struct {
	Role             string
	AllowedModels    []string
	AllowPassthrough bool
}

// BuiltinRoles returns the role policies required for the gateway to
// function: the admin role every bootstrap admin holds, and the auditor
// role. Demo roles (airllm_user) remain dev-seed-only.
func BuiltinRoles() []BuiltinRole {
	return []BuiltinRole{
		{Role: auth.AdminRole, AllowedModels: []string{"*"}, AllowPassthrough: true},
		{Role: auth.AuditorRole, AllowedModels: []string{}, AllowPassthrough: false},
	}
}

// EnsureBuiltinRoles inserts the built-in role policies if absent. It runs at
// every boot in every environment and never overwrites operator changes
// (ON CONFLICT DO NOTHING).
func EnsureBuiltinRoles(ctx context.Context, st *store.Store) error {
	for _, r := range BuiltinRoles() {
		if _, err := st.PG.Exec(ctx, `
			INSERT INTO roles_policy (role, allowed_models, allow_passthrough, limits)
			VALUES ($1, $2, $3, '{}'::jsonb)
			ON CONFLICT (role) DO NOTHING`,
			r.Role, r.AllowedModels, r.AllowPassthrough); err != nil {
			return fmt.Errorf("ensure builtin role %s: %w", r.Role, err)
		}
	}
	return nil
}
