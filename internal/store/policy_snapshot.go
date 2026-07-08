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
		rows, err := q.Query(ctx, `
			SELECT allowed_models, allow_passthrough, limits
			FROM roles_policy WHERE role = ANY($1)`, roles)
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
// the role. Runs at admin-edit frequency; the per-user loop is intentional
// (each user's snapshot merges their full role set).
func RebuildKeySnapshotsRole(ctx context.Context, q Querier, role string) error {
	rows, err := q.Query(ctx,
		`SELECT id::text FROM users WHERE $1 = ANY(roles)`, role)
	if err != nil {
		return fmt.Errorf("list users for role: %w", err)
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("scan user id: %w", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		if err := RebuildKeySnapshotsUser(ctx, q, id); err != nil {
			return err
		}
	}
	return nil
}
