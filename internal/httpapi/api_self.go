package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"time"

	"github.com/rromenskyi/ipsupport-airllm/internal/apikey"
	"github.com/rromenskyi/ipsupport-airllm/internal/policy"
)

// handleMe returns the caller's identity.
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	sess, _ := sessionFrom(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{
		"subject":    sess.principal.Subject,
		"email":      sess.principal.Email,
		"roles":      sess.principal.Roles,
		"is_admin":   sess.principal.IsAdmin(),
		"is_auditor": sess.principal.IsAuditor(),
	})
}

type keyView struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	Last4      string     `json:"last4"`
	Status     string     `json:"status"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
}

// handleListKeys lists the caller's own keys (never the hash or token).
func (s *Server) handleListKeys(w http.ResponseWriter, r *http.Request) {
	sess, _ := sessionFrom(r.Context())
	keys, err := s.queryKeys(r.Context(), `WHERE user_id = $1`, sess.userID)
	if err != nil {
		writeControlError(w, http.StatusInternalServerError, "failed to list keys")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": keys})
}

// handleCreateKey mints a new key for the caller, snapshotting the caller's
// effective role policy. The full token is returned exactly once.
func (s *Server) handleCreateKey(w http.ResponseWriter, r *http.Request) {
	sess, _ := sessionFrom(r.Context())
	var body struct {
		Name string `json:"name"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeControlError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if body.Name == "" {
		body.Name = "key"
	}

	snapshot, err := s.effectivePolicyJSON(r.Context(), sess.principal.Roles)
	if err != nil {
		writeControlError(w, http.StatusInternalServerError, "failed to resolve policy")
		return
	}

	k, err := apikey.Generate(s.cfg.Env)
	if err != nil {
		writeControlError(w, http.StatusInternalServerError, "failed to generate key")
		return
	}

	var id string
	err = s.st.PG.QueryRow(r.Context(), `
		INSERT INTO api_keys (user_id, name, hash, prefix, last4, policy_snapshot, status)
		VALUES ($1, $2, $3, $4, $5, $6, 'active')
		RETURNING id::text`,
		sess.userID, body.Name, k.Hash, k.Prefix, k.Last4, snapshot,
	).Scan(&id)
	if err != nil {
		writeControlError(w, http.StatusInternalServerError, "failed to create key")
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"id":     id,
		"name":   body.Name,
		"token":  k.Token, // shown once
		"prefix": k.Prefix,
		"last4":  k.Last4,
	})
}

// handleRevokeKey revokes one of the caller's own keys.
func (s *Server) handleRevokeKey(w http.ResponseWriter, r *http.Request) {
	sess, _ := sessionFrom(r.Context())
	id := r.PathValue("id")
	tag, err := s.st.PG.Exec(r.Context(),
		`UPDATE api_keys SET status = 'revoked' WHERE id = $1 AND user_id = $2`, id, sess.userID)
	if err != nil {
		writeControlError(w, http.StatusInternalServerError, "failed to revoke key")
		return
	}
	if tag.RowsAffected() == 0 {
		writeControlError(w, http.StatusNotFound, "key not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}

// handleUsage returns the caller's rolling usage totals.
func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	sess, _ := sessionFrom(r.Context())
	usage, err := s.usageWindows(r.Context(), `WHERE user_id = $1`, sess.userID)
	if err != nil {
		writeControlError(w, http.StatusInternalServerError, "failed to load usage")
		return
	}
	writeJSON(w, http.StatusOK, usage)
}

// queryKeys returns key views for a WHERE clause taking a single arg.
func (s *Server) queryKeys(ctx context.Context, where string, arg any) ([]keyView, error) {
	rows, err := s.st.PG.Query(ctx, `
		SELECT id::text, name, prefix, last4, status, created_at, last_used_at
		FROM api_keys `+where+` ORDER BY created_at DESC`, arg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []keyView{}
	for rows.Next() {
		var k keyView
		if err := rows.Scan(&k.ID, &k.Name, &k.Prefix, &k.Last4, &k.Status, &k.CreatedAt, &k.LastUsedAt); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

type windowUsage struct {
	Tokens  int64   `json:"tokens"`
	CostUSD float64 `json:"cost_usd"`
}

// usageWindows aggregates the ledger over the rolling windows for a WHERE
// clause taking a single arg, or no arg when where is empty.
func (s *Server) usageWindows(ctx context.Context, where string, args ...any) (map[string]windowUsage, error) {
	q := `
		SELECT
			COALESCE(SUM(prompt_tokens + completion_tokens) FILTER (WHERE ts > now() - interval '5 hours'), 0),
			COALESCE(SUM(cost_usd)                          FILTER (WHERE ts > now() - interval '5 hours'), 0),
			COALESCE(SUM(prompt_tokens + completion_tokens) FILTER (WHERE ts > now() - interval '24 hours'), 0),
			COALESCE(SUM(cost_usd)                          FILTER (WHERE ts > now() - interval '24 hours'), 0),
			COALESCE(SUM(prompt_tokens + completion_tokens) FILTER (WHERE ts > now() - interval '7 days'), 0),
			COALESCE(SUM(cost_usd)                          FILTER (WHERE ts > now() - interval '7 days'), 0)
		FROM usage_ledger ` + where

	var w5, w24, w7 windowUsage
	if err := s.st.PG.QueryRow(ctx, q, args...).Scan(
		&w5.Tokens, &w5.CostUSD, &w24.Tokens, &w24.CostUSD, &w7.Tokens, &w7.CostUSD,
	); err != nil {
		return nil, err
	}
	return map[string]windowUsage{"5h": w5, "24h": w24, "7d": w7}, nil
}

// effectivePolicyJSON merges the policies of the caller's roles into one
// snapshot: union of allowed models, OR of passthrough, first non-empty
// limits.
func (s *Server) effectivePolicyJSON(ctx context.Context, roles []string) ([]byte, error) {
	eff := policy.KeyPolicy{}
	if len(roles) > 0 {
		rows, err := s.st.PG.Query(ctx, `
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
