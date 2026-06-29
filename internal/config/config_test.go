package config

import (
	"crypto/sha256"
	"encoding/base64"
	"testing"
)

// setBase sets a minimal valid environment (only DATABASE_URL is required).
// t.Setenv restores the prior value and forbids t.Parallel, so each test runs
// against a clean, isolated environment.
func setBase(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://u:p@localhost:5432/db?sslmode=disable")
	t.Setenv("HTTP_ADDR", "")
	t.Setenv("REDIS_URL", "")
	t.Setenv("ENV", "")
	t.Setenv("AUTH_MODE", "")
	t.Setenv("AIRLLM_MASTER_KEY", "")
	t.Setenv("AIRLLM_SESSION_KEY", "")
}

func TestLoadDefaults(t *testing.T) {
	setBase(t)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.HTTPAddr != ":8080" {
		t.Errorf("HTTPAddr default = %q, want :8080", c.HTTPAddr)
	}
	if c.RedisURL != "redis://localhost:6379/0" {
		t.Errorf("RedisURL default = %q", c.RedisURL)
	}
	if c.Env != "dev" {
		t.Errorf("Env default = %q, want dev", c.Env)
	}
	if c.AuthMode != "mock" {
		t.Errorf("AuthMode default = %q, want mock", c.AuthMode)
	}
	// dev derives a deterministic insecure key so sealed creds survive restarts.
	if !c.MasterKeyDev {
		t.Error("dev env must derive a dev master key (MasterKeyDev=true)")
	}
	if len(c.MasterKey) != 32 {
		t.Errorf("MasterKey length = %d, want 32", len(c.MasterKey))
	}
	want := sha256.Sum256([]byte("airllm-dev-insecure-master-key"))
	if string(c.MasterKey) != string(want[:]) {
		t.Error("dev master key must be the documented deterministic value")
	}
}

func TestLoadRequiresDatabaseURL(t *testing.T) {
	setBase(t)
	t.Setenv("DATABASE_URL", "")
	if _, err := Load(); err == nil {
		t.Fatal("expected error when DATABASE_URL is empty")
	}
}

func TestLoadValidatesEnv(t *testing.T) {
	setBase(t)
	t.Setenv("ENV", "staging")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for invalid ENV")
	}
}

func TestLoadValidatesAuthMode(t *testing.T) {
	setBase(t)
	t.Setenv("AUTH_MODE", "basic")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for invalid AUTH_MODE")
	}
}

func TestLoadMasterKeyFromEnv(t *testing.T) {
	setBase(t)
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = byte(i)
	}
	t.Setenv("AIRLLM_MASTER_KEY", base64.StdEncoding.EncodeToString(raw))
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.MasterKeyDev {
		t.Error("an explicit key must not be flagged as a dev key")
	}
	if string(c.MasterKey) != string(raw) {
		t.Error("MasterKey must decode from AIRLLM_MASTER_KEY")
	}
}

func TestLoadMasterKeyRejectsBadBase64(t *testing.T) {
	setBase(t)
	t.Setenv("AIRLLM_MASTER_KEY", "not-base64!!!")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for non-base64 AIRLLM_MASTER_KEY")
	}
}

func TestLoadMasterKeyRejectsWrongLength(t *testing.T) {
	setBase(t)
	t.Setenv("AIRLLM_MASTER_KEY", base64.StdEncoding.EncodeToString([]byte("too-short")))
	if _, err := Load(); err == nil {
		t.Fatal("expected error for AIRLLM_MASTER_KEY that is not 32 bytes")
	}
}

func TestLoadProdRequiresMasterKey(t *testing.T) {
	setBase(t)
	t.Setenv("ENV", "prod")
	if _, err := Load(); err == nil {
		t.Fatal("expected error: AIRLLM_MASTER_KEY is required in prod")
	}
}

func TestSessionKeyDerivedAndStable(t *testing.T) {
	setBase(t)
	c1, _ := Load()
	c2, _ := Load()
	if len(c1.SessionKey) != 32 {
		t.Fatalf("session key length = %d", len(c1.SessionKey))
	}
	if string(c1.SessionKey) != string(c2.SessionKey) {
		t.Error("derived session key must be deterministic across loads")
	}
}

func TestSessionKeyOverride(t *testing.T) {
	setBase(t)
	raw := make([]byte, 32)
	t.Setenv("AIRLLM_SESSION_KEY", base64.StdEncoding.EncodeToString(raw))
	c, err := Load()
	if err != nil || string(c.SessionKey) != string(raw) {
		t.Fatalf("override not honored: err=%v", err)
	}
}
