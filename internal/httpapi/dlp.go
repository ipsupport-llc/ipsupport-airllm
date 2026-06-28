package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/dlp"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/llm"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/store"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/webhook"
)

// dlpConfig is the global DLP policy.
type dlpConfig struct {
	Enabled bool   `json:"enabled"`
	Action  string `json:"action"` // off | flag | redact | block
	// ScanResponses is reserved and intentionally NOT enforced. DLP governs
	// prompts only — the data the coding agent sends upstream. Model responses
	// are never scanned, redacted, or blocked (by design). Kept for forward
	// compatibility of the stored settings shape.
	ScanResponses bool `json:"scan_responses"`

	// Layer 2: optional BERT-NER sidecar for fuzzy/contextual PII.
	ModelEnabled  bool    `json:"model_enabled"`
	ModelURL      string  `json:"model_url"`
	ModelMinScore float64 `json:"model_min_score"`
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

// dlpResult carries the outcome of a DLP scan for use by the capture pipeline.
type dlpResult struct {
	Findings        []dlp.Finding   // flat list (per-message offsets; for HadIncident / index)
	MsgFindings     [][]dlp.Finding // per-message findings, indexed parallel to req.Messages
	HadIncident     bool
	AlreadyRedacted bool // true when action="redact" masked req.Messages in place
	// OriginalMessages preserves the pre-redaction messages so the capture
	// pipeline can build a raw-window body. Set only when action="redact"
	// (the case that mutates req.Messages in place and loses the original).
	OriginalMessages []llm.Message
}

// snapshotOriginals returns a copy of msgs when the DLP action will redact them
// in place ("redact"), so the capture pipeline can still build an un-redacted
// raw-window body; it returns nil for every other action (which leaves
// req.Messages untouched, so the caller already holds the originals). Message
// Content is an immutable string, so this shallow copy preserves the pre-mask
// text even after req.Messages[i].Content is later reassigned.
func snapshotOriginals(action string, msgs []llm.Message) []llm.Message {
	if action != "redact" {
		return nil
	}
	out := make([]llm.Message, len(msgs))
	copy(out, msgs)
	return out
}

// dlpEnforce scans the request messages — prompts only, by design (responses
// are never scanned; see dlpConfig.ScanResponses). On "redact" it masks secrets
// in place; on "block" it returns blocked=true with a client message. Detections
// are recorded and alerted regardless of action (except "off").
// It also returns a dlpResult for the capture pipeline (findings are collected
// across all messages; offsets are per-message and meaningful only in context).
func (s *Server) dlpEnforce(ctx context.Context, ak authedKey, ingress string, req *llm.ChatRequest) (blocked bool, message string, result dlpResult) {
	cfg := s.dlpCfg()
	if !cfg.Enabled || cfg.Action == "off" {
		return false, "", dlpResult{}
	}

	// Snapshot the original messages before any in-place redaction so the
	// capture pipeline can build an un-redacted raw-window body.
	original := snapshotOriginals(cfg.Action, req.Messages)

	labelSet := map[string]bool{}
	total := 0
	sample := ""
	var allFindings []dlp.Finding
	perMsg := make([][]dlp.Finding, len(req.Messages))
	for i := range req.Messages {
		content := req.Messages[i].Content
		if content == "" {
			continue
		}
		findings := dlp.Scan(content)
		if cfg.ModelEnabled && cfg.ModelURL != "" {
			mctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			mf, err := dlp.ModelScan(mctx, s.httpc, cfg.ModelURL, cfg.ModelMinScore, content)
			cancel()
			if err != nil {
				slog.Error("dlp model scan failed; deterministic layer only", "err", err)
			} else if len(mf) > 0 {
				findings = dlp.Merge(append(findings, mf...))
			}
		}
		if len(findings) == 0 {
			continue
		}
		perMsg[i] = findings
		allFindings = append(allFindings, findings...)
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
		return false, "", dlpResult{}
	}

	labels := sortedKeys(labelSet)
	s.recordDLP(ctx, ak, ingress, req.Model, actionPast(cfg.Action), labels, total, sample)

	if cfg.Action == "block" {
		return true, "request blocked: sensitive content detected (" + strings.Join(labels, ", ") + ")", dlpResult{}
	}
	return false, "", dlpResult{
		Findings:         allFindings,
		MsgFindings:      perMsg,
		HadIncident:      true,
		AlreadyRedacted:  cfg.Action == "redact",
		OriginalMessages: original,
	}
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
