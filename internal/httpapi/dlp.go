package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/rromenskyi/ipsupport-airllm/internal/dlp"
	"github.com/rromenskyi/ipsupport-airllm/internal/llm"
	"github.com/rromenskyi/ipsupport-airllm/internal/store"
	"github.com/rromenskyi/ipsupport-airllm/internal/webhook"
)

// dlpConfig is the global DLP policy.
type dlpConfig struct {
	Enabled       bool   `json:"enabled"`
	Action        string `json:"action"` // off | flag | redact | block
	ScanResponses bool   `json:"scan_responses"`
}

func defaultDLPConfig() dlpConfig {
	return dlpConfig{Enabled: true, Action: "redact"}
}

func validDLPAction(a string) bool {
	switch a {
	case "off", "flag", "redact", "block":
		return true
	}
	return false
}

// loadDLP reads the DLP config from settings into the atomic cache.
func (s *Server) loadDLP(ctx context.Context) {
	cfg := defaultDLPConfig()
	if raw, err := s.st.GetSetting(ctx, "dlp"); err == nil && len(raw) > 0 {
		_ = json.Unmarshal(raw, &cfg)
	}
	if !validDLPAction(cfg.Action) {
		cfg.Action = "off"
	}
	s.dlpPtr.Store(&cfg)
}

func (s *Server) dlpCfg() dlpConfig {
	if c := s.dlpPtr.Load(); c != nil {
		return *c
	}
	return defaultDLPConfig()
}

// dlpEnforce scans the request messages. On "redact" it masks secrets in place;
// on "block" it returns blocked=true with a client message. Detections are
// recorded and alerted regardless of action (except "off").
func (s *Server) dlpEnforce(ctx context.Context, ak authedKey, ingress string, req *llm.ChatRequest) (blocked bool, message string) {
	cfg := s.dlpCfg()
	if !cfg.Enabled || cfg.Action == "off" {
		return false, ""
	}

	labelSet := map[string]bool{}
	total := 0
	sample := ""
	for i := range req.Messages {
		content := req.Messages[i].Content
		if content == "" {
			continue
		}
		findings := dlp.Scan(content)
		if len(findings) == 0 {
			continue
		}
		total += len(findings)
		for _, l := range dlp.Labels(findings) {
			labelSet[l] = true
		}
		if sample == "" {
			sample = excerpt(dlp.Redact(content, findings))
		}
		if cfg.Action == "redact" {
			req.Messages[i].Content = dlp.Redact(content, findings)
		}
	}
	if total == 0 {
		return false, ""
	}

	labels := sortedKeys(labelSet)
	s.recordDLP(ctx, ak, ingress, req.Model, actionPast(cfg.Action), labels, total, sample)

	if cfg.Action == "block" {
		return true, "request blocked: sensitive content detected (" + strings.Join(labels, ", ") + ")"
	}
	return false, ""
}

func (s *Server) recordDLP(ctx context.Context, ak authedKey, ingress, alias, action string, labels []string, count int, sample string) {
	if err := s.st.RecordDLPIncident(ctx, store.DLPIncident{
		KeyID: ak.KeyID, UserID: ak.UserID, IngressProtocol: ingress, Alias: alias,
		Action: action, Labels: labels, MatchCount: count, Sample: sample,
	}); err != nil {
		slog.Error("dlp incident record failed", "err", err)
	}

	eps, err := s.st.WebhooksForEvent(ctx, "dlp.incident")
	if err != nil || len(eps) == 0 {
		return
	}
	payload, _ := json.Marshal(map[string]any{
		"event":       "dlp.incident",
		"ts":          time.Now().UTC().Format(time.RFC3339),
		"action":      action,
		"labels":      labels,
		"match_count": count,
		"alias":       alias,
		"ingress":     ingress,
		"sample":      sample,
		"user_id":     ak.UserID,
		"key_id":      ak.KeyID,
	})
	endpoints := make([]webhook.Endpoint, 0, len(eps))
	for _, e := range eps {
		endpoints = append(endpoints, webhook.Endpoint{URL: e.URL, Secret: e.Secret})
	}
	webhook.Send(endpoints, payload)
}

func actionPast(action string) string {
	switch action {
	case "block":
		return "blocked"
	case "redact":
		return "redacted"
	default:
		return "flagged"
	}
}

func excerpt(s string) string {
	const max = 240
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
