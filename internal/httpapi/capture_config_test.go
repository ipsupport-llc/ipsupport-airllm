package httpapi

import "testing"

func TestDefaultCaptureConfigDisabled(t *testing.T) {
	cfg := defaultCaptureConfig()
	if cfg.Enabled {
		t.Error("capture must be disabled by default")
	}
	if !cfg.Redact {
		t.Error("redact must be true by default")
	}
	if cfg.SampleRate != 0 {
		t.Errorf("default SampleRate must be 0, got %v", cfg.SampleRate)
	}
	if cfg.RetentionDays <= 0 {
		t.Errorf("default RetentionDays must be positive, got %v", cfg.RetentionDays)
	}
}

func TestCaptureConfigSampleRateClamp(t *testing.T) {
	cfg := captureConfig{SampleRate: 2.5}
	cfg = clampCaptureConfig(cfg)
	if cfg.SampleRate != 1 {
		t.Errorf("SampleRate >1 must clamp to 1, got %v", cfg.SampleRate)
	}

	cfg2 := captureConfig{SampleRate: -0.5}
	cfg2 = clampCaptureConfig(cfg2)
	if cfg2.SampleRate != 0 {
		t.Errorf("SampleRate <0 must clamp to 0, got %v", cfg2.SampleRate)
	}

	cfg3 := captureConfig{SampleRate: 0.5}
	cfg3 = clampCaptureConfig(cfg3)
	if cfg3.SampleRate != 0.5 {
		t.Errorf("valid SampleRate must pass through, got %v", cfg3.SampleRate)
	}
}
