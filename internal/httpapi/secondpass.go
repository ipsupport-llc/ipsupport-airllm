package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/rromenskyi/ipsupport-airllm/internal/llm"
)

// secondpassConfig is the second-pass DLP scan policy.
type secondpassConfig struct {
	Enabled     bool    `json:"enabled"`
	Model       string  `json:"model"`        // model alias to use for scanning
	IntervalSec int     `json:"interval_sec"` // ticker interval; applied at start
	MinScore    float64 `json:"min_score"`    // minimum LLM confidence to report a finding
}

func defaultSecondpassConfig() secondpassConfig {
	return secondpassConfig{
		Enabled:     false,
		Model:       "",
		IntervalSec: 60,
		MinScore:    0.7,
	}
}

// loadSecondpass reads the second-pass config from settings into the atomic cache.
func (s *Server) loadSecondpass(ctx context.Context) {
	cfg := defaultSecondpassConfig()
	if raw, err := s.st.GetSetting(ctx, "secondpass"); err == nil && len(raw) > 0 {
		_ = json.Unmarshal(raw, &cfg)
	}
	if cfg.MinScore < 0 {
		cfg.MinScore = 0
	}
	if cfg.MinScore > 1 {
		cfg.MinScore = 1
	}
	if cfg.IntervalSec <= 0 {
		cfg.IntervalSec = 60
	}
	s.secondpassPtr.Store(&cfg)
}

// secondpassCfg returns the current second-pass config.
func (s *Server) secondpassCfg() secondpassConfig {
	if c := s.secondpassPtr.Load(); c != nil {
		return *c
	}
	return defaultSecondpassConfig()
}

// SecondpassCfg returns the current second-pass config for use by main.
func (s *Server) SecondpassCfg() secondpassConfig { return s.secondpassCfg() }

// SecondpassEnabled reports whether the second-pass job is enabled.
func (s *Server) SecondpassEnabled() bool { return s.secondpassCfg().Enabled }

// SecondpassChat calls the configured model alias with prompt and returns the
// raw response text. It is used by the secondpass Job as the Chat closure.
// Returns an error when the second-pass is disabled, no model is configured,
// or the provider call fails.
func (s *Server) SecondpassChat(ctx context.Context, prompt string) (string, error) {
	cfg := s.secondpassCfg()
	if cfg.Model == "" {
		return "", errors.New("secondpass: no model configured")
	}
	plan, err := s.router.Resolve(ctx, cfg.Model, false)
	if err != nil {
		return "", fmt.Errorf("secondpass: resolve %q: %w", cfg.Model, err)
	}
	reg := s.reg()
	for _, t := range plan.Ordered(0, s.freeFunc(reg)) {
		e, ok := reg.Get(t.Provider)
		if !ok {
			continue
		}
		req := llm.ChatRequest{
			Model: t.UpstreamModel,
			Messages: []llm.Message{
				{Role: "user", Content: prompt},
			},
		}
		resp, err := e.Provider.Chat(ctx, req)
		if err != nil {
			return "", fmt.Errorf("secondpass: chat: %w", err)
		}
		if len(resp.Choices) > 0 {
			return resp.Choices[0].Message.Content, nil
		}
		return "", nil
	}
	return "", errors.New("secondpass: no available targets")
}

// handleAdminGetSecondpass returns the current second-pass config.
func (s *Server) handleAdminGetSecondpass(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.secondpassCfg())
}

// handleAdminPutSecondpass saves a new second-pass config.
func (s *Server) handleAdminPutSecondpass(w http.ResponseWriter, r *http.Request) {
	sess, _ := sessionFrom(r.Context())
	var body secondpassConfig
	if err := decodeJSON(r, &body); err != nil {
		writeControlError(w, http.StatusBadRequest, "invalid body")
		return
	}
	raw, _ := json.Marshal(body)
	if err := s.st.PutSetting(r.Context(), "secondpass", raw); err != nil {
		writeControlError(w, http.StatusInternalServerError, "failed to save secondpass config")
		return
	}
	s.loadSecondpass(r.Context())
	s.audit(r.Context(), sess.principal.Subject, "secondpass.put", "secondpass", map[string]any{
		"enabled": body.Enabled, "model": body.Model,
	})
	writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
}
