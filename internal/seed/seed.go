// Package seed inserts demo data for the local mock: a dev admin user, a
// permissive role policy, the mock provider, a model alias, and a single
// API key with a fixed, well-known token. It is idempotent and intended
// ONLY for ENV=dev + AUTH_MODE=mock.
package seed

import (
	"context"
	"fmt"

	"github.com/rromenskyi/ipsupport-airouter/internal/apikey"
	"github.com/rromenskyi/ipsupport-airouter/internal/store"
)

// DevToken is the fixed, well-known API key seeded in the local mock.
// It is NOT a secret and must never be used outside local development.
const DevToken = "air_dev_demo00000000000000000000000000000z"

// Dev seeds idempotent demo data and returns the dev API token.
func Dev(ctx context.Context, st *store.Store) (string, error) {
	var userID string
	if err := st.PG.QueryRow(ctx, `
		INSERT INTO users (subject, email, display, roles)
		VALUES ('dev-admin', 'dev@local', 'Dev Admin', ARRAY['airouter_admin'])
		ON CONFLICT (subject) DO UPDATE SET updated_at = now()
		RETURNING id::text`).Scan(&userID); err != nil {
		return "", fmt.Errorf("seed user: %w", err)
	}

	if _, err := st.PG.Exec(ctx, `
		INSERT INTO roles_policy (role, allowed_models, allow_passthrough, limits)
		VALUES ('airouter_admin', ARRAY['*'], true, '{}'::jsonb)
		ON CONFLICT (role) DO NOTHING`); err != nil {
		return "", fmt.Errorf("seed role policy: %w", err)
	}

	if _, err := st.PG.Exec(ctx, `
		INSERT INTO providers (name, kind, base_url, enabled)
		VALUES ('mock', 'mock', '', true)
		ON CONFLICT (name) DO NOTHING`); err != nil {
		return "", fmt.Errorf("seed provider: %w", err)
	}

	if _, err := st.PG.Exec(ctx, `
		INSERT INTO model_aliases (alias, protocol)
		VALUES ('mock-gpt', 'openai')
		ON CONFLICT (alias) DO NOTHING`); err != nil {
		return "", fmt.Errorf("seed alias: %w", err)
	}

	if _, err := st.PG.Exec(ctx, `
		INSERT INTO alias_targets (alias, priority, provider_name, upstream_model, upstream_protocol)
		SELECT 'mock-gpt', 0, 'mock', 'mock-model-1', 'openai'
		WHERE NOT EXISTS (SELECT 1 FROM alias_targets WHERE alias = 'mock-gpt')`); err != nil {
		return "", fmt.Errorf("seed alias target: %w", err)
	}

	// A fallback alias whose primary target fails (model contains "fail")
	// so routing fallback can be exercised end-to-end.
	if _, err := st.PG.Exec(ctx, `
		INSERT INTO model_aliases (alias, protocol)
		VALUES ('mock-fallback', 'openai')
		ON CONFLICT (alias) DO NOTHING`); err != nil {
		return "", fmt.Errorf("seed fallback alias: %w", err)
	}
	for _, t := range []struct {
		priority int
		model    string
	}{{0, "mock-fail"}, {1, "mock-model-1"}} {
		if _, err := st.PG.Exec(ctx, `
			INSERT INTO alias_targets (alias, priority, provider_name, upstream_model, upstream_protocol)
			SELECT 'mock-fallback', $1, 'mock', $2, 'openai'
			WHERE NOT EXISTS (SELECT 1 FROM alias_targets WHERE alias = 'mock-fallback' AND priority = $1)`,
			t.priority, t.model); err != nil {
			return "", fmt.Errorf("seed fallback target: %w", err)
		}
	}

	// Pricing for the mock upstream models so cost metering is non-zero.
	for _, p := range []struct {
		model         string
		input, output float64
	}{
		{"mock-model-1", 0.50, 1.50},
		{"custom-model-x", 1.00, 2.00},
	} {
		if _, err := st.PG.Exec(ctx, `
			INSERT INTO pricing (model, input_per_1m, output_per_1m)
			VALUES ($1, $2, $3)
			ON CONFLICT (model) DO UPDATE SET input_per_1m = EXCLUDED.input_per_1m, output_per_1m = EXCLUDED.output_per_1m`,
			p.model, p.input, p.output); err != nil {
			return "", fmt.Errorf("seed pricing: %w", err)
		}
	}

	k := apikey.Describe(DevToken)
	if _, err := st.PG.Exec(ctx, `
		INSERT INTO api_keys (user_id, name, hash, prefix, last4, policy_snapshot, status)
		VALUES ($1, 'dev demo key', $2, $3, $4,
			'{"allowed_models":["*"],"allow_passthrough":true,"limits":{}}'::jsonb, 'active')
		ON CONFLICT (hash) DO UPDATE SET policy_snapshot = EXCLUDED.policy_snapshot, status = 'active'`,
		userID, k.Hash, k.Prefix, k.Last4); err != nil {
		return "", fmt.Errorf("seed api key: %w", err)
	}

	return DevToken, nil
}
