package httpapi

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/capture"
)

// captureConfig is the global capture policy.
type captureConfig struct {
	Enabled       bool    `json:"enabled"`
	SampleRate    float64 `json:"sample_rate"`
	Redact        bool    `json:"redact"`
	RetentionDays int     `json:"retention_days"`
	RawTraining   bool    `json:"raw_training"`
	RawTTLHours   int     `json:"raw_ttl_hours"`
}

func defaultCaptureConfig() captureConfig {
	return captureConfig{
		Enabled:       false,
		SampleRate:    0,
		Redact:        true,
		RetentionDays: 30,
		RawTTLHours:   24,
	}
}

// clampCaptureConfig clamps SampleRate to [0, 1] and keeps the day/hour windows
// positive so a saved config can never disable the sweeper or set a zero TTL.
func clampCaptureConfig(cfg captureConfig) captureConfig {
	if cfg.SampleRate < 0 {
		cfg.SampleRate = 0
	}
	if cfg.SampleRate > 1 {
		cfg.SampleRate = 1
	}
	if cfg.RetentionDays <= 0 {
		cfg.RetentionDays = 30
	}
	if cfg.RawTTLHours <= 0 {
		cfg.RawTTLHours = 24
	}
	// The raw (un-redacted) copy must never outlive its row: otherwise the
	// retention sweep drops the row and orphans an un-redacted secret blob that
	// nothing references. Cap the raw TTL to the retention window.
	if maxRaw := cfg.RetentionDays * 24; cfg.RawTTLHours > maxRaw {
		cfg.RawTTLHours = maxRaw
	}
	return cfg
}

// loadCapture reads the capture config from settings into the atomic cache.
func (s *Server) loadCapture(ctx context.Context) {
	cfg := defaultCaptureConfig()
	if raw, err := s.st.GetSetting(ctx, "capture"); err == nil && len(raw) > 0 {
		_ = json.Unmarshal(raw, &cfg)
	}
	cfg = clampCaptureConfig(cfg)
	s.capturePtr.Store(&cfg)
}

// captureCfg returns the current capture config.
func (s *Server) captureCfg() captureConfig {
	if c := s.capturePtr.Load(); c != nil {
		return *c
	}
	return defaultCaptureConfig()
}

// handleAdminGetCapture returns the current capture config.
func (s *Server) handleAdminGetCapture(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.captureCfg())
}

// handleAdminPutCapture saves a new capture config. Changes take effect on the
// next enqueue/sweep without a restart (the pipeline reads CaptureCfg live).
func (s *Server) handleAdminPutCapture(w http.ResponseWriter, r *http.Request) {
	sess, _ := sessionFrom(r.Context())
	var body captureConfig
	if err := decodeJSON(r, &body); err != nil {
		writeControlError(w, http.StatusBadRequest, "invalid body")
		return
	}
	body = clampCaptureConfig(body)
	raw, _ := json.Marshal(body)
	if err := s.st.PutSetting(r.Context(), "capture", raw); err != nil {
		writeControlError(w, http.StatusInternalServerError, "failed to save capture config")
		return
	}
	s.loadCapture(r.Context())
	s.audit(r.Context(), sess.principal.Subject, "capture.put", "capture", map[string]any{
		"enabled": body.Enabled, "redact": body.Redact, "raw_training": body.RawTraining,
	})
	writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
}

// CaptureCfg returns the current capture config as a capture.Config, for use
// by the capture pipeline's cfg function in main.
func (s *Server) CaptureCfg() capture.Config {
	c := s.captureCfg()
	return capture.Config{
		Enabled:       c.Enabled,
		SampleRate:    c.SampleRate,
		Redact:        c.Redact,
		RetentionDays: c.RetentionDays,
		RawTraining:   c.RawTraining,
		RawTTLHours:   c.RawTTLHours,
	}
}
