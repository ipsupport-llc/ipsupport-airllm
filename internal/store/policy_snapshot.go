package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/policy"
)

// Querier is the subset of *pgxpool.Pool and pgx.Tx the snapshot code needs,
// so mutation + rebuild can share one transaction.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// EffectivePolicy merges the policies of roles into one snapshot: union of
// allowed models (sorted, deduped), OR of passthrough, first non-empty
// limits. Moved verbatim from httpapi's effectivePolicyJSON so key issue and
// snapshot rebuild share one source of truth.
func EffectivePolicy(ctx context.Context, q Querier, roles []string) ([]byte, error) {
	eff := policy.KeyPolicy{}
	if len(roles) > 0 {
		// ORDER BY role: roles_policy has no precedence/priority column, so
		// "first non-empty limits wins" below has no defined meaning across
		// an unordered scan — without this, which role's limits apply to a
		// multi-role user is decided by incidental physical row order, which
		// a VACUUM, a changed query plan, or a Postgres version bump could
		// silently flip for existing users with no code change and no log
		// line marking it. This does not define a real precedence model
		// (that's a product decision, not implied by this fix) — it only
		// makes the existing "first wins" rule reproducible: alphabetically
		// first by role name, consistently, instead of whatever the
		// database happens to return today.
		rows, err := q.Query(ctx, `
			SELECT allowed_models, allow_passthrough, limits
			FROM roles_policy WHERE role = ANY($1) ORDER BY role`, roles)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		modelSet := map[string]bool{}
		for rows.Next() {
			var models []string
			var passthrough bool
			var limits json.RawMessage
			if err := rows.Scan(&models, &passthrough, &limits); err != nil {
				return nil, err
			}
			for _, m := range models {
				modelSet[m] = true
			}
			eff.AllowPassthrough = eff.AllowPassthrough || passthrough
			if len(eff.Limits) == 0 && len(limits) > 0 && string(limits) != "{}" {
				eff.Limits = limits
			}
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		for m := range modelSet {
			eff.AllowedModels = append(eff.AllowedModels, m)
		}
		sort.Strings(eff.AllowedModels)
	}
	return json.Marshal(eff)
}

// RebuildKeySnapshotsUser recomputes one user's effective policy and applies
// it to their active keys. Revoked keys are left untouched.
func RebuildKeySnapshotsUser(ctx context.Context, q Querier, userID string) error {
	var roles []string
	if err := q.QueryRow(ctx,
		`SELECT roles FROM users WHERE id = $1`, userID).Scan(&roles); err != nil {
		return fmt.Errorf("load user roles: %w", err)
	}
	return applyKeySnapshot(ctx, q, userID, roles)
}

// applyKeySnapshot computes and applies one user's effective policy, given
// their already-known role set — shared by both rebuild paths so the role
// path (below) doesn't have to re-query roles it was just handed.
func applyKeySnapshot(ctx context.Context, q Querier, userID string, roles []string) error {
	snap, err := EffectivePolicy(ctx, q, roles)
	if err != nil {
		return fmt.Errorf("effective policy: %w", err)
	}
	if _, err := q.Exec(ctx, `
		UPDATE api_keys SET policy_snapshot = $2
		WHERE user_id = $1 AND status = 'active'`, userID, snap); err != nil {
		return fmt.Errorf("update key snapshots: %w", err)
	}
	return nil
}

// RebuildKeySnapshotsRole rebuilds the key snapshots of every user holding
// the role. Runs at admin-edit frequency; the per-user EffectivePolicy call
// is intentional (each user's snapshot merges their full role set) — but
// their role sets come back in the SAME initial query, not one extra
// "SELECT roles FROM users WHERE id = $1" per user the way routing through
// RebuildKeySnapshotsUser would.
func RebuildKeySnapshotsRole(ctx context.Context, q Querier, role string) error {
	rows, err := q.Query(ctx,
		`SELECT id::text, roles FROM users WHERE $1 = ANY(roles)`, role)
	if err != nil {
		return fmt.Errorf("list users for role: %w", err)
	}
	type userRoles struct {
		id    string
		roles []string
	}
	var users []userRoles
	for rows.Next() {
		var u userRoles
		if err := rows.Scan(&u.id, &u.roles); err != nil {
			rows.Close()
			return fmt.Errorf("scan user: %w", err)
		}
		users = append(users, u)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, u := range users {
		if err := applyKeySnapshot(ctx, q, u.id, u.roles); err != nil {
			return err
		}
	}
	return nil
}
