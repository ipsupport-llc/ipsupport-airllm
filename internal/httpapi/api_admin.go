package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/pricing"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/providers"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/store"
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
	s.mux.HandleFunc("GET /api/admin/usage/series", a(s.handleAdminUsageSeries))
	s.mux.HandleFunc("GET /api/admin/usage/breakdown", a(s.handleAdminUsageBreakdown))
	s.mux.HandleFunc("GET /api/admin/usage/recent", a(s.handleAdminUsageRecent))
	s.mux.HandleFunc("GET /api/admin/audit", a(s.handleAdminAudit))
	s.mux.HandleFunc("GET /api/admin/roles", a(s.handleAdminRoles))
	s.mux.HandleFunc("PUT /api/admin/roles/{role}", a(s.handleAdminPutRole))
	s.mux.HandleFunc("GET /api/admin/providers", a(s.handleAdminProviders))
	s.mux.HandleFunc("PUT /api/admin/providers/{name}", a(s.handleAdminPutProvider))
	s.mux.HandleFunc("GET /api/admin/providers/{name}/models", a(s.handleAdminProviderModels))
	s.mux.HandleFunc("GET /api/admin/aliases", a(s.handleAdminAliases))
	s.mux.HandleFunc("PUT /api/admin/aliases/{alias}", a(s.handleAdminPutAlias))
	s.mux.HandleFunc("DELETE /api/admin/aliases/{alias}", a(s.handleAdminDeleteAlias))
	s.mux.HandleFunc("GET /api/admin/pricing", a(s.handleAdminPricing))
	s.mux.HandleFunc("PUT /api/admin/pricing/{model}", a(s.handleAdminPutPricing))
	s.mux.HandleFunc("POST /api/admin/pricing/import/{provider}", a(s.handleAdminPricingImport))

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
	tx, err := s.st.PG.Begin(r.Context())
	if err != nil {
		writeControlError(w, http.StatusInternalServerError, "failed to save role")
		return
	}
	defer tx.Rollback(r.Context())
	if _, err := tx.Exec(r.Context(), `
		INSERT INTO roles_policy (role, allowed_models, allow_passthrough, limits)
		VALUES ($1, $2, $3, $4::jsonb)
		ON CONFLICT (role) DO UPDATE SET
			allowed_models = EXCLUDED.allowed_models,
			allow_passthrough = EXCLUDED.allow_passthrough,
			limits = EXCLUDED.limits,
			updated_at = now()`,
		role, body.AllowedModels, body.AllowPassthrough, string(body.Limits)); err != nil {
		writeControlError(w, http.StatusInternalServerError, "failed to save role")
		return
	}
	// Keep existing keys honest: re-snapshot every affected user's active
	// keys in the same transaction (the missing half of the original design).
	if err := store.RebuildKeySnapshotsRole(r.Context(), tx, role); err != nil {
		writeControlError(w, http.StatusInternalServerError, "failed to rebuild key snapshots")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeControlError(w, http.StatusInternalServerError, "failed to save role")
		return
	}
	s.audit(r.Context(), sess.principal.Subject, "role.put", role, body)
	writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
}

func (s *Server) handleAdminProviders(w http.ResponseWriter, r *http.Request) {
	rows, err := s.st.PG.Query(r.Context(),
		`SELECT name, kind, base_url, enabled, max_concurrency, (cred_enc IS NOT NULL), config FROM providers ORDER BY name`)
	if err != nil {
		writeControlError(w, http.StatusInternalServerError, "failed to list providers")
		return
	}
	defer rows.Close()
	// The credential itself is never returned, only whether one is stored:
	// reading the configuration must not disclose it.
	type provider struct {
		Name           string          `json:"name"`
		Kind           string          `json:"kind"`
		BaseURL        string          `json:"base_url"`
		Enabled        bool            `json:"enabled"`
		MaxConcurrency int             `json:"max_concurrency"`
		HasCredential  bool            `json:"has_credential"`
		Config         json.RawMessage `json:"config"`
	}
	out := []provider{}
	for rows.Next() {
		var p provider
		if err := rows.Scan(&p.Name, &p.Kind, &p.BaseURL, &p.Enabled, &p.MaxConcurrency, &p.HasCredential, &p.Config); err != nil {
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
		Kind           string          `json:"kind"`
		BaseURL        string          `json:"base_url"`
		Enabled        bool            `json:"enabled"`
		MaxConcurrency int             `json:"max_concurrency"`
		Config         json.RawMessage `json:"config"`          // kind-specific structured configuration
		APIKey         string          `json:"api_key"`         // blank = keep existing
		CredentialJSON string          `json:"credential_json"` // a service-account key; blank = keep existing
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
	// Both fields seal into the same column, so a request setting both leaves
	// it ambiguous which identity was meant. Say so rather than pick one.
	if body.APIKey != "" && body.CredentialJSON != "" {
		writeControlError(w, http.StatusBadRequest, "set api_key or credential_json, not both")
		return
	}

	// A save that omits the configuration keeps the one already stored, so
	// reading it is what "keep" means. A provider that does not exist yet has
	// none, which the COALESCE renders as the empty string.
	var stored string
	if err := s.st.PG.QueryRow(r.Context(),
		`SELECT COALESCE((SELECT config::text FROM providers WHERE name = $1), '')`, name,
	).Scan(&stored); err != nil {
		writeControlError(w, http.StatusInternalServerError, "failed to read provider")
		return
	}
	config, err := providerConfigToStore(body.Kind, body.Config, stored, body.BaseURL)
	if err != nil {
		writeControlError(w, http.StatusBadRequest, err.Error())
		return
	}

	// nil leaves whatever credential is already stored in place (see the
	// COALESCE below); a non-nil value replaces it.
	var sealed []byte
	cred := body.APIKey
	if body.CredentialJSON != "" {
		cred = body.CredentialJSON
	}
	if cred != "" {
		sealed, err = s.sealer.Seal([]byte(cred))
		if err != nil {
			writeControlError(w, http.StatusInternalServerError, "failed to seal credential")
			return
		}
	}
	if _, err := s.st.PG.Exec(r.Context(), `
		INSERT INTO providers (name, kind, base_url, enabled, max_concurrency, config, cred_enc)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (name) DO UPDATE SET
			kind = EXCLUDED.kind, base_url = EXCLUDED.base_url, enabled = EXCLUDED.enabled,
			max_concurrency = EXCLUDED.max_concurrency, config = EXCLUDED.config,
			cred_enc = COALESCE(EXCLUDED.cred_enc, providers.cred_enc), updated_at = now()`,
		name, body.Kind, body.BaseURL, body.Enabled, body.MaxConcurrency, config, sealed,
	); err != nil {
		writeControlError(w, http.StatusInternalServerError, "failed to save provider")
		return
	}

	// Apply immediately (rebuild the registry with new creds/limits/clients).
	if err := s.reloadProviders(r.Context()); err != nil {
		slog.Error("provider reload failed", "err", err)
	}

	// Audit without the secret. The configuration is not one — it holds a
	// cloud project and location, both of which the operator just typed.
	s.audit(r.Context(), sess.principal.Subject, "provider.put", name, map[string]any{
		"kind": body.Kind, "base_url": body.BaseURL, "enabled": body.Enabled,
		"max_concurrency": body.MaxConcurrency, "config": json.RawMessage(config),
		"has_key": sealed != nil,
	})
	writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
}

// providerConfigToStore decides which structured configuration a save should
// persist, and rejects one that could never serve a request.
//
// A supplied configuration replaces what was stored; an omitted one keeps it.
// That is deliberately unlike base_url or enabled, which this endpoint
// replaces wholesale, and deliberately like the credential: a client that
// does not know a kind has configuration at all — today's console, or a
// script toggling "enabled" — would otherwise erase the cloud project a
// Vertex provider is addressed by, as a side effect of an unrelated edit.
//
// Rejecting an incomplete configuration here is the other half: the
// alternative is an operator learning about the mistake from a failed request
// hours later, with nothing in the form to suggest what was wrong.
func providerConfigToStore(kind string, supplied json.RawMessage, stored, baseURL string) (string, error) {
	config := strings.TrimSpace(string(supplied))
	if config == "" || config == "null" {
		config = strings.TrimSpace(stored)
	}
	if config == "" {
		config = "{}"
	}
	if err := providers.ValidateProviderConfig(kind, []byte(config), baseURL); err != nil {
		return "", err
	}
	return config, nil
}

// aliasTarget has no upstream_protocol field: it's derived from the
// provider's own kind (see routing.Router.Resolve), never operator-chosen —
// alias_targets.upstream_protocol is legacy, unread schema kept only to
// avoid a migration.
type aliasTarget struct {
	Priority      int    `json:"priority"`
	Provider      string `json:"provider"`
	UpstreamModel string `json:"upstream_model"`
	DisplayLabel  string `json:"display_label"`
}

func (s *Server) handleAdminAliases(w http.ResponseWriter, r *http.Request) {
	rows, err := s.st.PG.Query(r.Context(), `
		SELECT a.alias, a.protocol, a.strategy, a.dlp_model_scan, a.expose_backend_headers, a.dlp_audio_scan,
			COALESCE(t.priority, 0), COALESCE(t.provider_name, ''),
			COALESCE(t.upstream_model, ''), COALESCE(t.display_label, '')
		FROM model_aliases a
		LEFT JOIN alias_targets t ON t.alias = a.alias
		ORDER BY a.alias, t.priority`)
	if err != nil {
		writeControlError(w, http.StatusInternalServerError, "failed to list aliases")
		return
	}
	defer rows.Close()
	type aliasView struct {
		Alias                string        `json:"alias"`
		Protocol             string        `json:"protocol"`
		Strategy             string        `json:"strategy"`
		DLPModelScan         bool          `json:"dlp_model_scan"`
		ExposeBackendHeaders bool          `json:"expose_backend_headers"`
		DLPAudioScan         bool          `json:"dlp_audio_scan"`
		Targets              []aliasTarget `json:"targets"`
	}
	byAlias := map[string]*aliasView{}
	var order []string
	for rows.Next() {
		var alias, protocol, strategy, provider, upModel, label string
		var priority int
		var dlpModelScan, exposeBackendHeaders, dlpAudioScan bool
		if err := rows.Scan(&alias, &protocol, &strategy, &dlpModelScan, &exposeBackendHeaders, &dlpAudioScan, &priority, &provider, &upModel, &label); err != nil {
			writeControlError(w, http.StatusInternalServerError, "failed to read aliases")
			return
		}
		av, ok := byAlias[alias]
		if !ok {
			av = &aliasView{Alias: alias, Protocol: protocol, Strategy: strategy, DLPModelScan: dlpModelScan, ExposeBackendHeaders: exposeBackendHeaders, DLPAudioScan: dlpAudioScan, Targets: []aliasTarget{}}
			byAlias[alias] = av
			order = append(order, alias)
		}
		if provider != "" {
			av.Targets = append(av.Targets, aliasTarget{priority, provider, upModel, label})
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
		Protocol             string        `json:"protocol"`
		Strategy             string        `json:"strategy"`
		DLPModelScan         *bool         `json:"dlp_model_scan"`
		ExposeBackendHeaders bool          `json:"expose_backend_headers"`
		DLPAudioScan         *bool         `json:"dlp_audio_scan"`
		Targets              []aliasTarget `json:"targets"`
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
	scan := true
	if body.DLPModelScan != nil {
		scan = *body.DLPModelScan
	}
	audioScan := true
	if body.DLPAudioScan != nil {
		audioScan = *body.DLPAudioScan
	}

	tx, err := s.st.PG.Begin(r.Context())
	if err != nil {
		writeControlError(w, http.StatusInternalServerError, "failed to save alias")
		return
	}
	defer tx.Rollback(r.Context())

	if _, err := tx.Exec(r.Context(), `
		INSERT INTO model_aliases (alias, protocol, strategy, dlp_model_scan, expose_backend_headers, dlp_audio_scan) VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (alias) DO UPDATE SET protocol = EXCLUDED.protocol, strategy = EXCLUDED.strategy, dlp_model_scan = EXCLUDED.dlp_model_scan, expose_backend_headers = EXCLUDED.expose_backend_headers, dlp_audio_scan = EXCLUDED.dlp_audio_scan`,
		alias, body.Protocol, body.Strategy, scan, body.ExposeBackendHeaders, audioScan); err != nil {
		writeControlError(w, http.StatusInternalServerError, "failed to save alias")
		return
	}
	if _, err := tx.Exec(r.Context(), `DELETE FROM alias_targets WHERE alias = $1`, alias); err != nil {
		writeControlError(w, http.StatusInternalServerError, "failed to save alias")
		return
	}
	for _, t := range body.Targets {
		// upstream_protocol has no operator-supplied value anymore (see the
		// aliasTarget doc comment) — the column is NOT NULL with no default,
		// so it still needs something; the literal value written here is
		// never read back by anything.
		if _, err := tx.Exec(r.Context(), `
			INSERT INTO alias_targets (alias, priority, provider_name, upstream_model, upstream_protocol, display_label)
			VALUES ($1, $2, $3, $4, 'unused', $5)`, alias, t.Priority, t.Provider, t.UpstreamModel, t.DisplayLabel); err != nil {
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

// priceRow is the wire shape of a pricing row, shared by the list and save
// handlers so the two cannot drift. The context-tier fields are always present:
// an untiered row reports a zero threshold, which is what it is.
//
// Model is the row's identity and the save path takes it from the URL, so a
// body carrying a different one is checked rather than quietly discarded.
type priceRow struct {
	Provider    string  `json:"provider"`
	Model       string  `json:"model"`
	InputPer1M  float64 `json:"input_per_1m"`
	OutputPer1M float64 `json:"output_per_1m"`
	Unit        string  `json:"unit"`

	ContextThreshold int     `json:"context_threshold"`
	InputPer1MAbove  float64 `json:"input_per_1m_above"`
	OutputPer1MAbove float64 `json:"output_per_1m_above"`
}

func (s *Server) handleAdminPricing(w http.ResponseWriter, r *http.Request) {
	rows, err := s.st.PG.Query(r.Context(),
		`SELECT provider, model, input_per_1m, output_per_1m, unit,
			context_threshold, input_per_1m_above, output_per_1m_above
		 FROM pricing ORDER BY provider, model`)
	if err != nil {
		writeControlError(w, http.StatusInternalServerError, "failed to list pricing")
		return
	}
	defer rows.Close()
	out := []priceRow{}
	for rows.Next() {
		var p priceRow
		if err := rows.Scan(&p.Provider, &p.Model, &p.InputPer1M, &p.OutputPer1M, &p.Unit,
			&p.ContextThreshold, &p.InputPer1MAbove, &p.OutputPer1MAbove); err != nil {
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
	var body priceRow
	if err := decodeJSON(r, &body); err != nil {
		writeControlError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if body.Model != "" && body.Model != model {
		writeControlError(w, http.StatusBadRequest, "model in body does not match the URL")
		return
	}
	if body.Unit == "" {
		body.Unit = pricing.UnitTokens
	}
	p := pricing.Price{
		InputPer1M: body.InputPer1M, OutputPer1M: body.OutputPer1M, Unit: body.Unit,
		ContextThreshold: body.ContextThreshold,
		InputPer1MAbove:  body.InputPer1MAbove, OutputPer1MAbove: body.OutputPer1MAbove,
	}
	// Rejected here rather than stored and discovered on a bill: a threshold
	// with a missing rate above it would price a long prompt at nothing.
	if err := p.Validate(); err != nil {
		writeControlError(w, http.StatusBadRequest, err.Error())
		return
	}
	_, err := s.st.PG.Exec(r.Context(), `
		INSERT INTO pricing (provider, model, input_per_1m, output_per_1m, unit,
			context_threshold, input_per_1m_above, output_per_1m_above)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (provider, model) DO UPDATE SET
			input_per_1m = EXCLUDED.input_per_1m, output_per_1m = EXCLUDED.output_per_1m,
			unit = EXCLUDED.unit, context_threshold = EXCLUDED.context_threshold,
			input_per_1m_above = EXCLUDED.input_per_1m_above,
			output_per_1m_above = EXCLUDED.output_per_1m_above, updated_at = now()`,
		body.Provider, model, body.InputPer1M, body.OutputPer1M, body.Unit,
		body.ContextThreshold, body.InputPer1MAbove, body.OutputPer1MAbove)
	if err != nil {
		writeControlError(w, http.StatusInternalServerError, "failed to save pricing")
		return
	}
	s.pricing.Set(body.Provider, model, p)
	s.audit(r.Context(), sess.principal.Subject, "pricing.put", model, body)
	writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
}

// handleAdminUsageSeries returns global hourly usage buckets.
func (s *Server) handleAdminUsageSeries(w http.ResponseWriter, r *http.Request) {
	hours := clampHours(r.URL.Query().Get("hours"))
	series, err := s.usageSeries(r.Context(), ``, hours)
	if err != nil {
		writeControlError(w, http.StatusInternalServerError, "failed to load usage series")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"series": series})
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
