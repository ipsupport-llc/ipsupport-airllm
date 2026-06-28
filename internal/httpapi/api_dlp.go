package httpapi

import (
	"encoding/json"
	"net/http"
	"time"
)

func (s *Server) handleAdminGetDLP(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.dlpCfg())
}

func (s *Server) handleAdminPutDLP(w http.ResponseWriter, r *http.Request) {
	sess, _ := sessionFrom(r.Context())
	var body dlpConfig
	if err := decodeJSON(r, &body); err != nil {
		writeControlError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if !validDLPAction(body.Action) {
		writeControlError(w, http.StatusBadRequest, "action must be off | flag | redact | block")
		return
	}
	raw, _ := json.Marshal(body)
	if err := s.st.PutSetting(r.Context(), "dlp", raw); err != nil {
		writeControlError(w, http.StatusInternalServerError, "failed to save DLP config")
		return
	}
	s.loadDLP(r.Context())
	s.audit(r.Context(), sess.principal.Subject, "dlp.put", "dlp", body)
	writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
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
		`SELECT id::text, name, url, events, enabled, (secret <> '') FROM webhooks ORDER BY created_at`)
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
	if len(body.Events) == 0 {
		body.Events = []string{"dlp.incident"}
	}
	var id string
	if err := s.st.PG.QueryRow(r.Context(), `
		INSERT INTO webhooks (name, url, secret, events, enabled)
		VALUES ($1, $2, $3, $4, $5) RETURNING id::text`,
		body.Name, body.URL, body.Secret, body.Events, body.Enabled,
	).Scan(&id); err != nil {
		writeControlError(w, http.StatusInternalServerError, "failed to create webhook")
		return
	}
	s.audit(r.Context(), sess.principal.Subject, "webhook.create", id, map[string]any{
		"name": body.Name, "url": body.URL, "events": body.Events, "enabled": body.Enabled, "has_secret": body.Secret != "",
	})
	writeJSON(w, http.StatusCreated, map[string]string{"id": id, "status": "created"})
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
