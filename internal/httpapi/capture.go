package httpapi

import (
	"context"
	"encoding/json"

	"github.com/rromenskyi/ipsupport-airllm/internal/capture"
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

// clampCaptureConfig clamps SampleRate to [0, 1].
func clampCaptureConfig(cfg captureConfig) captureConfig {
	if cfg.SampleRate < 0 {
		cfg.SampleRate = 0
	}
	if cfg.SampleRate > 1 {
		cfg.SampleRate = 1
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

// CaptureCfg returns the current capture config as a capture.Config, for use
// by the capture pipeline's cfg function in main.
func (s *Server) CaptureCfg() capture.Config {
	c := s.captureCfg()
	return capture.Config{
		Enabled:       c.Enabled,
		SampleRate:    c.SampleRate,
		Redact:        c.Redact,
		RetentionDays: c.RetentionDays,
	}
}
