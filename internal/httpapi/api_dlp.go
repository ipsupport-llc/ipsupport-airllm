package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"time"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/dlp"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/webhook"
)

func (s *Server) handleAdminGetDLP(w http.ResponseWriter, _ *http.Request) {
	cfg := s.dlpCfg()
	cfg.Patterns = effectivePatternsForDisplay(cfg)
	writeJSON(w, http.StatusOK, cfg)
}

// handleAdminDLPPatterns returns the catalog of toggleable detection patterns
// (built-ins + the model-based PII toggles) so the console can render the
// Sensitive Info Detection panel.
func (s *Server) handleAdminDLPPatterns(w http.ResponseWriter, _ *http.Request) {
	type info struct {
		Label     string `json:"label"`
		Category  string `json:"category"`
		DefaultOn bool   `json:"default_on"`
		Model     bool   `json:"model"` // requires the BERT sidecar; adds latency
	}
	out := []info{}
	for _, p := range dlp.BuiltinPatterns() {
		out = append(out, info{p.Label, p.Category, p.DefaultOn, false})
	}
	out = append(out,
		info{"person_name", "pii", false, true},
		info{"address", "pii", false, true},
		info{"organization", "pii", false, true},
		info{"other", "pii", true, true}, // catch-all for unmapped model labels (e.g. CoNLL's MISC)
	)
	writeJSON(w, http.StatusOK, map[string]any{"patterns": out})
}

func (s *Server) handleAdminPutDLP(w http.ResponseWriter, r *http.Request) {
	sess, _ := sessionFrom(r.Context())
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeControlError(w, http.StatusBadRequest, "invalid body")
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	var body dlpConfig
	if err := decodeJSON(r, &body); err != nil {
		writeControlError(w, http.StatusBadRequest, "invalid body")
		return
	}
	keepOmittedDLPFields(raw, &body, s.dlpCfg())
	if !validDLPAction(body.Action) {
		writeControlError(w, http.StatusBadRequest, "action must be off | flag | redact | block")
		return
	}
	if err := validatePatternLabels(body.Patterns); err != nil {
		writeControlError(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := compileCustomPatterns(body.CustomPatterns); err != nil {
		writeControlError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateModelConfig(body); err != nil {
		writeControlError(w, http.StatusBadRequest, err.Error())
		return
	}
	stored, _ := json.Marshal(body)
	if err := s.st.PutSetting(r.Context(), "dlp", stored); err != nil {
		writeControlError(w, http.StatusInternalServerError, "failed to save DLP config")
		return
	}
	s.loadDLP(r.Context())
	s.audit(r.Context(), sess.principal.Subject, "dlp.put", "dlp", body)
	writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
}

// keepOmittedDLPFields carries every stored field over into a PUT body that
// does not mention it, so a partial PUT such as {"action":"block"} changes
// only the action instead of zeroing model_url, model_urls, the scan budget,
// the pattern toggles and the rest. A key that is present — even as "", 0,
// false, null, [] or {} — is an explicit replacement. Keys are matched the
// way encoding/json matched them when decoding body (case-insensitively).
func keepOmittedDLPFields(raw []byte, body *dlpConfig, cur dlpConfig) {
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		return
	}
	present := func(name string) bool {
		for k := range keys {
			if strings.EqualFold(k, name) {
				return true
			}
		}
		return false
	}
	bv, cv := reflect.ValueOf(body).Elem(), reflect.ValueOf(cur)
	for i := 0; i < bv.NumField(); i++ {
		name, _, _ := strings.Cut(bv.Type().Field(i).Tag.Get("json"), ",")
		if name == "" || name == "-" { // not serialized (compiledCustom)
			continue
		}
		if !present(name) {
			bv.Field(i).Set(cv.Field(i))
		}
	}
}

func (s *Server) handleAdminDLPIncidents(w http.ResponseWriter, r *http.Request) {
	rows, err := s.st.PG.Query(r.Context(), `
		SELECT i.ts, COALESCE(u.subject, ''), i.ingress_protocol, i.alias,
			i.action, i.labels, i.match_count, i.sample
		FROM dlp_incidents i
		LEFT JOIN users u ON u.id = i.user_id
		ORDER BY i.ts DESC LIMIT 200`)
	if err != nil {
		writeControlError(w, http.StatusInternalServerError, "failed to load incidents")
		return
	}
	defer rows.Close()
	type incident struct {
		TS      time.Time `json:"ts"`
		User    string    `json:"user"`
		Ingress string    `json:"ingress"`
		Alias   string    `json:"alias"`
		Action  string    `json:"action"`
		Labels  []string  `json:"labels"`
		Matches int       `json:"match_count"`
		Sample  string    `json:"sample"`
	}
	out := []incident{}
	for rows.Next() {
		var i incident
		if err := rows.Scan(&i.TS, &i.User, &i.Ingress, &i.Alias, &i.Action, &i.Labels, &i.Matches, &i.Sample); err != nil {
			writeControlError(w, http.StatusInternalServerError, "failed to read incidents")
			return
		}
		out = append(out, i)
	}
	writeJSON(w, http.StatusOK, map[string]any{"incidents": out})
}

func (s *Server) handleAdminWebhooks(w http.ResponseWriter, r *http.Request) {
	rows, err := s.st.PG.Query(r.Context(),
		`SELECT id::text, name, url, events, enabled, (secret_enc IS NOT NULL) FROM webhooks ORDER BY created_at`)
	if err != nil {
		writeControlError(w, http.StatusInternalServerError, "failed to list webhooks")
		return
	}
	defer rows.Close()
	type hook struct {
		ID        string   `json:"id"`
		Name      string   `json:"name"`
		URL       string   `json:"url"`
		Events    []string `json:"events"`
		Enabled   bool     `json:"enabled"`
		HasSecret bool     `json:"has_secret"`
	}
	out := []hook{}
	for rows.Next() {
		var h hook
		if err := rows.Scan(&h.ID, &h.Name, &h.URL, &h.Events, &h.Enabled, &h.HasSecret); err != nil {
			writeControlError(w, http.StatusInternalServerError, "failed to read webhooks")
			return
		}
		out = append(out, h)
	}
	writeJSON(w, http.StatusOK, map[string]any{"webhooks": out})
}

func (s *Server) handleAdminCreateWebhook(w http.ResponseWriter, r *http.Request) {
	sess, _ := sessionFrom(r.Context())
	var body struct {
		Name    string   `json:"name"`
		URL     string   `json:"url"`
		Secret  string   `json:"secret"`
		Events  []string `json:"events"`
		Enabled bool     `json:"enabled"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeControlError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if body.URL == "" {
		writeControlError(w, http.StatusBadRequest, "url is required")
		return
	}
	if err := webhook.ValidateURL(r.Context(), body.URL); err != nil {
		writeControlError(w, http.StatusBadRequest, "invalid webhook url: "+err.Error())
		return
	}
	if len(body.Events) == 0 {
		body.Events = []string{"dlp.incident"}
	}
	var secretEnc []byte
	if body.Secret != "" {
		sealed, err := s.sealer.Seal([]byte(body.Secret))
		if err != nil {
			writeControlError(w, http.StatusInternalServerError, "failed to seal webhook secret")
			return
		}
		secretEnc = sealed
	}
	var id string
	if err := s.st.PG.QueryRow(r.Context(), `
		INSERT INTO webhooks (name, url, secret_enc, events, enabled)
		VALUES ($1, $2, $3, $4, $5) RETURNING id::text`,
		body.Name, body.URL, secretEnc, body.Events, body.Enabled,
	).Scan(&id); err != nil {
		writeControlError(w, http.StatusInternalServerError, "failed to create webhook")
		return
	}
	s.audit(r.Context(), sess.principal.Subject, "webhook.create", id, map[string]any{
		"name": body.Name, "url": body.URL, "events": body.Events, "enabled": body.Enabled, "has_secret": body.Secret != "",
	})
	writeJSON(w, http.StatusCreated, map[string]string{"id": id, "status": "created"})
}

// handleAdminUpdateWebhook replaces a webhook's name, URL, events and
// enabled flag, with the same validation as create. The secret cannot be
// read back, so an empty secret keeps the stored one; a non-empty one is
// sealed and replaces it.
func (s *Server) handleAdminUpdateWebhook(w http.ResponseWriter, r *http.Request) {
	sess, _ := sessionFrom(r.Context())
	id := r.PathValue("id")
	var body struct {
		Name    string   `json:"name"`
		URL     string   `json:"url"`
		Secret  string   `json:"secret"`
		Events  []string `json:"events"`
		Enabled bool     `json:"enabled"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeControlError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if body.URL == "" {
		writeControlError(w, http.StatusBadRequest, "url is required")
		return
	}
	if err := webhook.ValidateURL(r.Context(), body.URL); err != nil {
		writeControlError(w, http.StatusBadRequest, "invalid webhook url: "+err.Error())
		return
	}
	if len(body.Events) == 0 {
		body.Events = []string{"dlp.incident"}
	}
	var secretEnc []byte // nil keeps the stored secret (COALESCE below)
	if body.Secret != "" {
		sealed, err := s.sealer.Seal([]byte(body.Secret))
		if err != nil {
			writeControlError(w, http.StatusInternalServerError, "failed to seal webhook secret")
			return
		}
		secretEnc = sealed
	}
	tag, err := s.st.PG.Exec(r.Context(), `
		UPDATE webhooks SET name = $2, url = $3, events = $4, enabled = $5,
			secret_enc = COALESCE($6, secret_enc)
		WHERE id = $1`,
		id, body.Name, body.URL, body.Events, body.Enabled, secretEnc)
	if err != nil {
		writeControlError(w, http.StatusInternalServerError, "failed to update webhook")
		return
	}
	if tag.RowsAffected() == 0 {
		writeControlError(w, http.StatusNotFound, "webhook not found")
		return
	}
	s.audit(r.Context(), sess.principal.Subject, "webhook.update", id, map[string]any{
		"name": body.Name, "url": body.URL, "events": body.Events, "enabled": body.Enabled, "secret_changed": body.Secret != "",
	})
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

func (s *Server) handleAdminDeleteWebhook(w http.ResponseWriter, r *http.Request) {
	sess, _ := sessionFrom(r.Context())
	id := r.PathValue("id")
	tag, err := s.st.PG.Exec(r.Context(), `DELETE FROM webhooks WHERE id = $1`, id)
	if err != nil {
		writeControlError(w, http.StatusInternalServerError, "failed to delete webhook")
		return
	}
	if tag.RowsAffected() == 0 {
		writeControlError(w, http.StatusNotFound, "webhook not found")
		return
	}
	s.audit(r.Context(), sess.principal.Subject, "webhook.delete", id, nil)
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}
