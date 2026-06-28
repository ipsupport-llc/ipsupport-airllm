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

func TestCaptureConfigWindowClamp(t *testing.T) {
	// Zero/negative retention and raw TTL must clamp to safe defaults so a saved
	// config can never disable the sweeper or set a zero-length raw window.
	cfg := clampCaptureConfig(captureConfig{RetentionDays: 0, RawTTLHours: -5})
	if cfg.RetentionDays != 30 {
		t.Errorf("RetentionDays <=0 must clamp to 30, got %v", cfg.RetentionDays)
	}
	if cfg.RawTTLHours != 24 {
		t.Errorf("RawTTLHours <=0 must clamp to 24, got %v", cfg.RawTTLHours)
	}

	cfg2 := clampCaptureConfig(captureConfig{RetentionDays: 7, RawTTLHours: 2})
	if cfg2.RetentionDays != 7 || cfg2.RawTTLHours != 2 {
		t.Errorf("positive windows must pass through, got %d / %d", cfg2.RetentionDays, cfg2.RawTTLHours)
	}

	// The raw TTL must never exceed the retention window, or the retention sweep
	// would orphan an un-redacted raw blob.
	cfg3 := clampCaptureConfig(captureConfig{RetentionDays: 1, RawTTLHours: 100})
	if cfg3.RawTTLHours != 24 {
		t.Errorf("RawTTLHours must be capped at RetentionDays*24 (24), got %d", cfg3.RawTTLHours)
	}
}
