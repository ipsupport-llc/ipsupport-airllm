package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/dlp"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/llm"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/modelpool"
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
	// ModelURLs is the sidecar endpoint list; when empty the single ModelURL is
	// used (back-compat). ModelMaxConcurrency caps concurrent scans per endpoint
	// (0 = unlimited). See internal/modelpool.
	ModelURLs           []string `json:"model_urls,omitempty"`
	ModelMaxConcurrency int      `json:"model_max_concurrency,omitempty"`

	// Sensitive Info Detection (guardrails). Patterns maps a built-in pattern
	// label (including the model toggles person_name/address/organization and
	// high_entropy) to on/off; a label absent from the map uses its default, so
	// a nil/partial map keeps legacy behavior. CustomPatterns are operator-defined.
	Patterns       map[string]bool `json:"patterns,omitempty"`
	CustomPatterns []customPattern `json:"custom_patterns,omitempty"`

	// compiledCustom holds the enabled CustomPatterns compiled once at load. Not
	// serialized; rebuilt by loadDLP.
	compiledCustom []dlp.CustomPattern
}

// customPattern is an operator-defined detection rule stored as a regex string.
type customPattern struct {
	Label   string `json:"label"`
	Regex   string `json:"regex"`
	Enabled bool   `json:"enabled"`
}

const (
	maxCustomPatterns = 50
	maxCustomRegexLen = 512
	maxCustomLabelLen = 64
)

// validateCustom checks one enabled custom pattern and returns its compiled
// regex. A regex that matches the empty string is rejected: it would emit a
// zero-width finding at every position and corrupt the redacted prompt.
func validateCustom(c customPattern) (*regexp.Regexp, error) {
	if strings.TrimSpace(c.Label) == "" {
		return nil, fmt.Errorf("custom pattern label is required")
	}
	if len(c.Label) > maxCustomLabelLen {
		return nil, fmt.Errorf("custom pattern label too long (max %d)", maxCustomLabelLen)
	}
	if len(c.Regex) == 0 || len(c.Regex) > maxCustomRegexLen {
		return nil, fmt.Errorf("custom pattern %q regex must be 1..%d chars", c.Label, maxCustomRegexLen)
	}
	re, err := regexp.Compile(c.Regex)
	if err != nil {
		return nil, fmt.Errorf("custom pattern %q: %w", c.Label, err)
	}
	if re.MatchString("") {
		return nil, fmt.Errorf("custom pattern %q must not match the empty string", c.Label)
	}
	return re, nil
}

// compileCustomPatterns validates and compiles the ENABLED custom patterns. It
// returns the compiled valid ones plus the first validation error (if any), so
// PUT can reject an invalid set while loadDLP keeps every valid pattern even if
// a hand-edited settings row contains a bad one. Go's RE2 engine is linear-time,
// so operator regexes cannot cause catastrophic backtracking.
func compileCustomPatterns(cps []customPattern) ([]dlp.CustomPattern, error) {
	if len(cps) > maxCustomPatterns {
		return nil, fmt.Errorf("too many custom patterns (max %d)", maxCustomPatterns)
	}
	var out []dlp.CustomPattern
	var firstErr error
	for _, c := range cps {
		if !c.Enabled {
			continue
		}
		re, err := validateCustom(c)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		out = append(out, dlp.CustomPattern{Label: c.Label, Re: re})
	}
	return out, firstErr
}

// dlpToggle reads a pattern toggle, falling back to def when unset.
func dlpToggle(enabled map[string]bool, label string, def bool) bool {
	if enabled != nil {
		if v, ok := enabled[label]; ok {
			return v
		}
	}
	return def
}

// modelToggleLabel maps a model entity label ("pii:<ENTITY>") to its toggle,
// normalizing the common NER vocabularies (abbreviated and full, any case) so
// the toggle works whether the sidecar emits PER or PERSON, LOC or LOCATION/GPE,
// ORG or ORGANIZATION. An unrecognized entity returns "" (kept, untoggled).
func modelToggleLabel(label string) string {
	switch strings.ToUpper(strings.TrimPrefix(label, "pii:")) {
	case "PER", "PERSON", "PERS":
		return "person_name"
	case "LOC", "LOCATION", "GPE", "ADDRESS":
		return "address"
	case "ORG", "ORGANIZATION", "ORGANISATION":
		return "organization"
	}
	return ""
}

// filterModelFindings drops model PII findings whose toggle is off. Model PII
// is opt-in (default off). When the operator has not configured toggles
// (enabled == nil) all findings are kept, preserving pre-guardrails behavior.
func filterModelFindings(fs []dlp.Finding, enabled map[string]bool) []dlp.Finding {
	if enabled == nil {
		return fs
	}
	out := fs[:0]
	for _, f := range fs {
		toggle := modelToggleLabel(f.Label)
		if toggle == "" || dlpToggle(enabled, toggle, false) {
			out = append(out, f)
		}
	}
	return out
}

// knownPatternLabels is the set of valid toggle keys: every built-in pattern
// plus the model toggles.
func knownPatternLabels() map[string]bool {
	known := map[string]bool{"person_name": true, "address": true, "organization": true}
	for _, info := range dlp.BuiltinPatterns() {
		known[info.Label] = true
	}
	return known
}

// validatePatternLabels rejects unknown pattern keys so a typo can't silently
// disable nothing (or be mistaken for a real toggle).
func validatePatternLabels(p map[string]bool) error {
	known := knownPatternLabels()
	for k := range p {
		if !known[k] {
			return fmt.Errorf("unknown pattern %q", k)
		}
	}
	return nil
}

func defaultDLPConfig() dlpConfig {
	return dlpConfig{Enabled: true, Action: "redact"}
}

// effectiveModelURLs returns the configured sidecar URLs, falling back to the
// single ModelURL for back-compat. Blank entries are dropped.
func (c dlpConfig) effectiveModelURLs() []string {
	var out []string
	for _, u := range c.ModelURLs {
		if u = strings.TrimSpace(u); u != "" {
			out = append(out, u)
		}
	}
	if len(out) == 0 {
		if u := strings.TrimSpace(c.ModelURL); u != "" {
			out = append(out, u)
		}
	}
	return out
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
	// Stored config is already validated; ignore the error and use whatever
	// compiled (defensive against a hand-edited settings row).
	cfg.compiledCustom, _ = compileCustomPatterns(cfg.CustomPatterns)
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
// modelScan gates only the layer-2 BERT sidecar scan (per-alias passthrough
// toggle); the layer-1 deterministic patterns always run regardless.
func (s *Server) dlpEnforce(ctx context.Context, ak authedKey, ingress string, req *llm.ChatRequest, modelScan bool) (blocked bool, message string, result dlpResult) {
	cfg := s.dlpCfg()
	if !cfg.Enabled || cfg.Action == "off" {
		return false, "", dlpResult{}
	}

	// Snapshot the original messages before any in-place redaction so the
	// capture pipeline can build an un-redacted raw-window body.
	original := snapshotOriginals(cfg.Action, req.Messages)

	modelOn := cfg.ModelEnabled && modelScan && len(cfg.effectiveModelURLs()) > 0
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
		findings := dlp.ScanWith(content, dlp.PatternSet{
			Enabled: cfg.Patterns,
			Custom:  cfg.compiledCustom,
			Entropy: dlpToggle(cfg.Patterns, "high_entropy", true),
		})
		if modelOn {
			mctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			mstart := time.Now()
			mf, err := s.modelPool.Scan(mctx, s.httpc, content, cfg.ModelMinScore)
			cancel()
			switch {
			case errors.Is(err, modelpool.ErrAllBusy):
				s.metrics.DLPModelSkipped("all_busy")
			case errors.Is(err, modelpool.ErrNoEndpoints):
				s.metrics.DLPModelSkipped("no_endpoints")
			case err != nil:
				s.metrics.DLPModelObserve(time.Since(mstart))
				slog.Error("dlp model scan failed; deterministic layer only", "err", err)
			default:
				s.metrics.DLPModelObserve(time.Since(mstart))
				if mf = filterModelFindings(mf, cfg.Patterns); len(mf) > 0 {
					findings = dlp.Merge(append(findings, mf...))
				}
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
