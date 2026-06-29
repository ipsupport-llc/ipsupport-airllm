package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/pricing"
)

func (s *Server) adminRoutes() {
	a := s.requireAdmin
	s.mux.HandleFunc("GET /api/admin/users", a(s.handleAdminUsers))
	s.mux.HandleFunc("POST /api/admin/users", a(s.handleCreateUser))
	s.mux.HandleFunc("PUT /api/admin/users/{id}", a(s.handleUpdateUser))
	s.mux.HandleFunc("POST /api/admin/users/{id}/password", a(s.handleResetPassword))
	s.mux.HandleFunc("DELETE /api/admin/users/{id}", a(s.handleDeleteUser))
	s.mux.HandleFunc("GET /api/admin/keys", a(s.handleAdminKeys))
	s.mux.HandleFunc("POST /api/admin/keys/{id}/revoke", a(s.handleAdminRevokeKey))
	s.mux.HandleFunc("GET /api/admin/usage", a(s.handleAdminUsage))
	s.mux.HandleFunc("GET /api/admin/audit", a(s.handleAdminAudit))
	s.mux.HandleFunc("GET /api/admin/roles", a(s.handleAdminRoles))
	s.mux.HandleFunc("PUT /api/admin/roles/{role}", a(s.handleAdminPutRole))
	s.mux.HandleFunc("GET /api/admin/providers", a(s.handleAdminProviders))
	s.mux.HandleFunc("PUT /api/admin/providers/{name}", a(s.handleAdminPutProvider))
	s.mux.HandleFunc("GET /api/admin/aliases", a(s.handleAdminAliases))
	s.mux.HandleFunc("PUT /api/admin/aliases/{alias}", a(s.handleAdminPutAlias))
	s.mux.HandleFunc("DELETE /api/admin/aliases/{alias}", a(s.handleAdminDeleteAlias))
	s.mux.HandleFunc("GET /api/admin/pricing", a(s.handleAdminPricing))
	s.mux.HandleFunc("PUT /api/admin/pricing/{model}", a(s.handleAdminPutPricing))

	// Dataset export for DLP model fine-tuning.
	s.mux.HandleFunc("POST /api/admin/dataset/export", a(s.handleAdminDatasetExport))

	// DLP: config, pattern catalog, incidents, alert webhooks.
	s.mux.HandleFunc("GET /api/admin/dlp", a(s.handleAdminGetDLP))
	s.mux.HandleFunc("PUT /api/admin/dlp", a(s.handleAdminPutDLP))
	s.mux.HandleFunc("GET /api/admin/dlp/patterns", a(s.handleAdminDLPPatterns))

	// Capture: data-capture policy (sampling, redaction, retention, raw window).
	s.mux.HandleFunc("GET /api/admin/capture", a(s.handleAdminGetCapture))
	s.mux.HandleFunc("PUT /api/admin/capture", a(s.handleAdminPutCapture))

	// Second-pass: background DLP re-scan config.
	s.mux.HandleFunc("GET /api/admin/secondpass", a(s.handleAdminGetSecondpass))
	s.mux.HandleFunc("PUT /api/admin/secondpass", a(s.handleAdminPutSecondpass))
	s.mux.HandleFunc("GET /api/admin/dlp/incidents", a(s.handleAdminDLPIncidents))
	s.mux.HandleFunc("GET /api/admin/webhooks", a(s.handleAdminWebhooks))
	s.mux.HandleFunc("POST /api/admin/webhooks", a(s.handleAdminCreateWebhook))
	s.mux.HandleFunc("DELETE /api/admin/webhooks/{id}", a(s.handleAdminDeleteWebhook))
}

func (s *Server) handleAdminUsers(w http.ResponseWriter, r *http.Request) {
	rows, err := s.st.PG.Query(r.Context(),
		`SELECT id::text, subject, email, display, roles, disabled, auth_source, created_at FROM users ORDER BY created_at`)
	if err != nil {
		writeControlError(w, http.StatusInternalServerError, "failed to list users")
		return
	}
	defer rows.Close()
	type user struct {
		ID         string    `json:"id"`
		Subject    string    `json:"subject"`
		Email      string    `json:"email"`
		Display    string    `json:"display"`
		Roles      []string  `json:"roles"`
		Disabled   bool      `json:"disabled"`
		AuthSource string    `json:"auth_source"`
		CreatedAt  time.Time `json:"created_at"`
	}
	out := []user{}
	for rows.Next() {
		var u user
		if err := rows.Scan(&u.ID, &u.Subject, &u.Email, &u.Display, &u.Roles, &u.Disabled, &u.AuthSource, &u.CreatedAt); err != nil {
			writeControlError(w, http.StatusInternalServerError, "failed to read users")
			return
		}
		out = append(out, u)
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": out})
}

func (s *Server) handleAdminKeys(w http.ResponseWriter, r *http.Request) {
	rows, err := s.st.PG.Query(r.Context(), `
		SELECT k.id::text, u.subject, k.name, k.prefix, k.last4, k.status, k.created_at, k.last_used_at
		FROM api_keys k JOIN users u ON u.id = k.user_id
		ORDER BY k.created_at DESC`)
	if err != nil {
		writeControlError(w, http.StatusInternalServerError, "failed to list keys")
		return
	}
	defer rows.Close()
	type adminKey struct {
		ID         string     `json:"id"`
		Owner      string     `json:"owner"`
		Name       string     `json:"name"`
		Prefix     string     `json:"prefix"`
		Last4      string     `json:"last4"`
		Status     string     `json:"status"`
		CreatedAt  time.Time  `json:"created_at"`
		LastUsedAt *time.Time `json:"last_used_at"`
	}
	out := []adminKey{}
	for rows.Next() {
		var k adminKey
		if err := rows.Scan(&k.ID, &k.Owner, &k.Name, &k.Prefix, &k.Last4, &k.Status, &k.CreatedAt, &k.LastUsedAt); err != nil {
			writeControlError(w, http.StatusInternalServerError, "failed to read keys")
			return
		}
		out = append(out, k)
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": out})
}

func (s *Server) handleAdminRevokeKey(w http.ResponseWriter, r *http.Request) {
	sess, _ := sessionFrom(r.Context())
	id := r.PathValue("id")
	tag, err := s.st.PG.Exec(r.Context(), `UPDATE api_keys SET status = 'revoked' WHERE id = $1`, id)
	if err != nil {
		writeControlError(w, http.StatusInternalServerError, "failed to revoke key")
		return
	}
	if tag.RowsAffected() == 0 {
		writeControlError(w, http.StatusNotFound, "key not found")
		return
	}
	s.audit(r.Context(), sess.principal.Subject, "key.revoke", id, nil)
	writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}

func (s *Server) handleAdminUsage(w http.ResponseWriter, r *http.Request) {
	usage, err := s.usageWindows(r.Context(), "")
	if err != nil {
		writeControlError(w, http.StatusInternalServerError, "failed to load usage")
		return
	}
	writeJSON(w, http.StatusOK, usage)
}

func (s *Server) handleAdminAudit(w http.ResponseWriter, r *http.Request) {
	rows, err := s.st.PG.Query(r.Context(),
		`SELECT ts, actor, action, target, detail FROM audit_log ORDER BY ts DESC LIMIT 200`)
	if err != nil {
		writeControlError(w, http.StatusInternalServerError, "failed to load audit")
		return
	}
	defer rows.Close()
	type entry struct {
		TS     time.Time       `json:"ts"`
		Actor  string          `json:"actor"`
		Action string          `json:"action"`
		Target string          `json:"target"`
		Detail json.RawMessage `json:"detail"`
	}
	out := []entry{}
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.TS, &e.Actor, &e.Action, &e.Target, &e.Detail); err != nil {
			writeControlError(w, http.StatusInternalServerError, "failed to read audit")
			return
		}
		out = append(out, e)
	}
	writeJSON(w, http.StatusOK, map[string]any{"audit": out})
}

type rolePolicyView struct {
	Role             string          `json:"role"`
	AllowedModels    []string        `json:"allowed_models"`
	AllowPassthrough bool            `json:"allow_passthrough"`
	Limits           json.RawMessage `json:"limits"`
}

func (s *Server) handleAdminRoles(w http.ResponseWriter, r *http.Request) {
	rows, err := s.st.PG.Query(r.Context(),
		`SELECT role, allowed_models, allow_passthrough, limits FROM roles_policy ORDER BY role`)
	if err != nil {
		writeControlError(w, http.StatusInternalServerError, "failed to list roles")
		return
	}
	defer rows.Close()
	out := []rolePolicyView{}
	for rows.Next() {
		var rp rolePolicyView
		if err := rows.Scan(&rp.Role, &rp.AllowedModels, &rp.AllowPassthrough, &rp.Limits); err != nil {
			writeControlError(w, http.StatusInternalServerError, "failed to read roles")
			return
		}
		out = append(out, rp)
	}
	writeJSON(w, http.StatusOK, map[string]any{"roles": out})
}

func (s *Server) handleAdminPutRole(w http.ResponseWriter, r *http.Request) {
	sess, _ := sessionFrom(r.Context())
	role := r.PathValue("role")
	var body struct {
		AllowedModels    []string        `json:"allowed_models"`
		AllowPassthrough bool            `json:"allow_passthrough"`
		Limits           json.RawMessage `json:"limits"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeControlError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if len(body.Limits) == 0 {
		body.Limits = json.RawMessage("{}")
	}
	_, err := s.st.PG.Exec(r.Context(), `
		INSERT INTO roles_policy (role, allowed_models, allow_passthrough, limits)
		VALUES ($1, $2, $3, $4::jsonb)
		ON CONFLICT (role) DO UPDATE SET
			allowed_models = EXCLUDED.allowed_models,
			allow_passthrough = EXCLUDED.allow_passthrough,
			limits = EXCLUDED.limits,
			updated_at = now()`,
		role, body.AllowedModels, body.AllowPassthrough, string(body.Limits))
	if err != nil {
		writeControlError(w, http.StatusInternalServerError, "failed to save role")
		return
	}
	s.audit(r.Context(), sess.principal.Subject, "role.put", role, body)
	writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
}

func (s *Server) handleAdminProviders(w http.ResponseWriter, r *http.Request) {
	rows, err := s.st.PG.Query(r.Context(),
		`SELECT name, kind, base_url, enabled, max_concurrency, (cred_enc IS NOT NULL) FROM providers ORDER BY name`)
	if err != nil {
		writeControlError(w, http.StatusInternalServerError, "failed to list providers")
		return
	}
	defer rows.Close()
	type provider struct {
		Name           string `json:"name"`
		Kind           string `json:"kind"`
		BaseURL        string `json:"base_url"`
		Enabled        bool   `json:"enabled"`
		MaxConcurrency int    `json:"max_concurrency"`
		HasCredential  bool   `json:"has_credential"`
	}
	out := []provider{}
	for rows.Next() {
		var p provider
		if err := rows.Scan(&p.Name, &p.Kind, &p.BaseURL, &p.Enabled, &p.MaxConcurrency, &p.HasCredential); err != nil {
			writeControlError(w, http.StatusInternalServerError, "failed to read providers")
			return
		}
		out = append(out, p)
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": out})
}

func (s *Server) handleAdminPutProvider(w http.ResponseWriter, r *http.Request) {
	sess, _ := sessionFrom(r.Context())
	name := r.PathValue("name")
	var body struct {
		Kind           string `json:"kind"`
		BaseURL        string `json:"base_url"`
		Enabled        bool   `json:"enabled"`
		MaxConcurrency int    `json:"max_concurrency"`
		APIKey         string `json:"api_key"` // blank = keep existing
	}
	if err := decodeJSON(r, &body); err != nil {
		writeControlError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if body.Kind == "" {
		writeControlError(w, http.StatusBadRequest, "kind is required")
		return
	}
	if body.MaxConcurrency < 0 {
		body.MaxConcurrency = 0
	}

	if body.APIKey != "" {
		sealed, err := s.sealer.Seal([]byte(body.APIKey))
		if err != nil {
			writeControlError(w, http.StatusInternalServerError, "failed to seal credential")
			return
		}
		_, err = s.st.PG.Exec(r.Context(), `
			INSERT INTO providers (name, kind, base_url, enabled, max_concurrency, cred_enc)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (name) DO UPDATE SET
				kind = EXCLUDED.kind, base_url = EXCLUDED.base_url, enabled = EXCLUDED.enabled,
				max_concurrency = EXCLUDED.max_concurrency, cred_enc = EXCLUDED.cred_enc, updated_at = now()`,
			name, body.Kind, body.BaseURL, body.Enabled, body.MaxConcurrency, sealed)
		if err != nil {
			writeControlError(w, http.StatusInternalServerError, "failed to save provider")
			return
		}
	} else {
		// No key supplied: leave any existing credential untouched.
		_, err := s.st.PG.Exec(r.Context(), `
			INSERT INTO providers (name, kind, base_url, enabled, max_concurrency)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (name) DO UPDATE SET
				kind = EXCLUDED.kind, base_url = EXCLUDED.base_url, enabled = EXCLUDED.enabled,
				max_concurrency = EXCLUDED.max_concurrency, updated_at = now()`,
			name, body.Kind, body.BaseURL, body.Enabled, body.MaxConcurrency)
		if err != nil {
			writeControlError(w, http.StatusInternalServerError, "failed to save provider")
			return
		}
	}

	// Apply immediately (rebuild the registry with new creds/limits/clients).
	if err := s.reloadProviders(r.Context()); err != nil {
		slog.Error("provider reload failed", "err", err)
	}

	// Audit without the secret.
	s.audit(r.Context(), sess.principal.Subject, "provider.put", name, map[string]any{
		"kind": body.Kind, "base_url": body.BaseURL, "enabled": body.Enabled,
		"max_concurrency": body.MaxConcurrency, "has_key": body.APIKey != "",
	})
	writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
}

type aliasTarget struct {
	Priority         int    `json:"priority"`
	Provider         string `json:"provider"`
	UpstreamModel    string `json:"upstream_model"`
	UpstreamProtocol string `json:"upstream_protocol"`
}

func (s *Server) handleAdminAliases(w http.ResponseWriter, r *http.Request) {
	rows, err := s.st.PG.Query(r.Context(), `
		SELECT a.alias, a.protocol, a.strategy,
			COALESCE(t.priority, 0), COALESCE(t.provider_name, ''),
			COALESCE(t.upstream_model, ''), COALESCE(t.upstream_protocol, '')
		FROM model_aliases a
		LEFT JOIN alias_targets t ON t.alias = a.alias
		ORDER BY a.alias, t.priority`)
	if err != nil {
		writeControlError(w, http.StatusInternalServerError, "failed to list aliases")
		return
	}
	defer rows.Close()
	type aliasView struct {
		Alias    string        `json:"alias"`
		Protocol string        `json:"protocol"`
		Strategy string        `json:"strategy"`
		Targets  []aliasTarget `json:"targets"`
	}
	byAlias := map[string]*aliasView{}
	var order []string
	for rows.Next() {
		var alias, protocol, strategy, provider, upModel, upProto string
		var priority int
		if err := rows.Scan(&alias, &protocol, &strategy, &priority, &provider, &upModel, &upProto); err != nil {
			writeControlError(w, http.StatusInternalServerError, "failed to read aliases")
			return
		}
		av, ok := byAlias[alias]
		if !ok {
			av = &aliasView{Alias: alias, Protocol: protocol, Strategy: strategy, Targets: []aliasTarget{}}
			byAlias[alias] = av
			order = append(order, alias)
		}
		if provider != "" {
			av.Targets = append(av.Targets, aliasTarget{priority, provider, upModel, upProto})
		}
	}
	out := make([]aliasView, 0, len(order))
	for _, a := range order {
		out = append(out, *byAlias[a])
	}
	writeJSON(w, http.StatusOK, map[string]any{"aliases": out})
}

func (s *Server) handleAdminPutAlias(w http.ResponseWriter, r *http.Request) {
	sess, _ := sessionFrom(r.Context())
	alias := r.PathValue("alias")
	var body struct {
		Protocol string        `json:"protocol"`
		Strategy string        `json:"strategy"`
		Targets  []aliasTarget `json:"targets"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeControlError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if body.Protocol == "" {
		body.Protocol = "openai"
	}
	if body.Strategy != "least_busy" {
		body.Strategy = "round_robin"
	}

	tx, err := s.st.PG.Begin(r.Context())
	if err != nil {
		writeControlError(w, http.StatusInternalServerError, "failed to save alias")
		return
	}
	defer tx.Rollback(r.Context())

	if _, err := tx.Exec(r.Context(), `
		INSERT INTO model_aliases (alias, protocol, strategy) VALUES ($1, $2, $3)
		ON CONFLICT (alias) DO UPDATE SET protocol = EXCLUDED.protocol, strategy = EXCLUDED.strategy`,
		alias, body.Protocol, body.Strategy); err != nil {
		writeControlError(w, http.StatusInternalServerError, "failed to save alias")
		return
	}
	if _, err := tx.Exec(r.Context(), `DELETE FROM alias_targets WHERE alias = $1`, alias); err != nil {
		writeControlError(w, http.StatusInternalServerError, "failed to save alias")
		return
	}
	for _, t := range body.Targets {
		proto := t.UpstreamProtocol
		if proto == "" {
			proto = "openai"
		}
		if _, err := tx.Exec(r.Context(), `
			INSERT INTO alias_targets (alias, priority, provider_name, upstream_model, upstream_protocol)
			VALUES ($1, $2, $3, $4, $5)`, alias, t.Priority, t.Provider, t.UpstreamModel, proto); err != nil {
			writeControlError(w, http.StatusBadRequest, "invalid target (provider must exist): "+err.Error())
			return
		}
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeControlError(w, http.StatusInternalServerError, "failed to save alias")
		return
	}
	s.audit(r.Context(), sess.principal.Subject, "alias.put", alias, body)
	writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
}

func (s *Server) handleAdminDeleteAlias(w http.ResponseWriter, r *http.Request) {
	sess, _ := sessionFrom(r.Context())
	alias := r.PathValue("alias")
	tag, err := s.st.PG.Exec(r.Context(), `DELETE FROM model_aliases WHERE alias = $1`, alias)
	if err != nil {
		writeControlError(w, http.StatusInternalServerError, "failed to delete alias")
		return
	}
	if tag.RowsAffected() == 0 {
		writeControlError(w, http.StatusNotFound, "alias not found")
		return
	}
	s.audit(r.Context(), sess.principal.Subject, "alias.delete", alias, nil)
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handleAdminPricing(w http.ResponseWriter, r *http.Request) {
	rows, err := s.st.PG.Query(r.Context(),
		`SELECT model, input_per_1m, output_per_1m FROM pricing ORDER BY model`)
	if err != nil {
		writeControlError(w, http.StatusInternalServerError, "failed to list pricing")
		return
	}
	defer rows.Close()
	type price struct {
		Model       string  `json:"model"`
		InputPer1M  float64 `json:"input_per_1m"`
		OutputPer1M float64 `json:"output_per_1m"`
	}
	out := []price{}
	for rows.Next() {
		var p price
		if err := rows.Scan(&p.Model, &p.InputPer1M, &p.OutputPer1M); err != nil {
			writeControlError(w, http.StatusInternalServerError, "failed to read pricing")
			return
		}
		out = append(out, p)
	}
	writeJSON(w, http.StatusOK, map[string]any{"pricing": out})
}

func (s *Server) handleAdminPutPricing(w http.ResponseWriter, r *http.Request) {
	sess, _ := sessionFrom(r.Context())
	model := r.PathValue("model")
	var body struct {
		InputPer1M  float64 `json:"input_per_1m"`
		OutputPer1M float64 `json:"output_per_1m"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeControlError(w, http.StatusBadRequest, "invalid body")
		return
	}
	_, err := s.st.PG.Exec(r.Context(), `
		INSERT INTO pricing (model, input_per_1m, output_per_1m)
		VALUES ($1, $2, $3)
		ON CONFLICT (model) DO UPDATE SET
			input_per_1m = EXCLUDED.input_per_1m, output_per_1m = EXCLUDED.output_per_1m, updated_at = now()`,
		model, body.InputPer1M, body.OutputPer1M)
	if err != nil {
		writeControlError(w, http.StatusInternalServerError, "failed to save pricing")
		return
	}
	s.pricing.Set(model, pricing.Price{InputPer1M: body.InputPer1M, OutputPer1M: body.OutputPer1M})
	s.audit(r.Context(), sess.principal.Subject, "pricing.put", model, body)
	writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
}

// audit records a control-plane mutation. Best-effort. When auditHook is set
// (e.g. in tests) it is called instead of writing to the database.
func (s *Server) audit(ctx context.Context, actor, action, target string, detail any) {
	if s.auditHook != nil {
		s.auditHook(ctx, actor, action, target, detail)
		return
	}
	b, err := json.Marshal(detail)
	if err != nil || len(b) == 0 || string(b) == "null" {
		b = []byte("{}")
	}
	if _, err := s.st.PG.Exec(ctx,
		`INSERT INTO audit_log (actor, action, target, detail) VALUES ($1, $2, $3, $4::jsonb)`,
		actor, action, target, string(b)); err != nil {
		slog.Error("audit write failed", "err", err, "action", action, "target", target)
	}
}
