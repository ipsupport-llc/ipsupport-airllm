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
	// Clear OIDC vars so they don't bleed into non-OIDC tests.
	t.Setenv("OIDC_ISSUER", "")
	t.Setenv("OIDC_CLIENT_ID", "")
	t.Setenv("OIDC_CLIENT_SECRET", "")
	t.Setenv("OIDC_REDIRECT_URL", "")
	t.Setenv("OIDC_ROLES_CLAIM", "")
	t.Setenv("OIDC_SCOPES", "")
	t.Setenv("OIDC_ROLE_MAP", "")
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
	if c.AuthMode != "local" {
		t.Errorf("AuthMode default = %q, want local", c.AuthMode)
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

func TestAuthModeNormalizesMockToLocal(t *testing.T) {
	setBase(t)
	t.Setenv("AUTH_MODE", "mock")
	c, err := Load()
	if err != nil || c.AuthMode != "local" {
		t.Fatalf("mock must normalize to local, got %q err=%v", c.AuthMode, err)
	}
}

func TestAuthModeRejectsUnknown(t *testing.T) {
	setBase(t)
	t.Setenv("AUTH_MODE", "ldap")
	if _, err := Load(); err == nil {
		t.Fatal("unknown AUTH_MODE must error")
	}
}

func TestOIDCModeRequiresVars(t *testing.T) {
	setBase(t)
	t.Setenv("AUTH_MODE", "oidc")
	// No OIDC vars set — must error.
	if _, err := Load(); err == nil {
		t.Fatal("AUTH_MODE=oidc without OIDC vars must error")
	}
}

func TestOIDCModeWithAllVars(t *testing.T) {
	setBase(t)
	t.Setenv("AUTH_MODE", "oidc")
	t.Setenv("OIDC_ISSUER", "https://idp.example.com")
	t.Setenv("OIDC_CLIENT_ID", "client-id")
	t.Setenv("OIDC_CLIENT_SECRET", "client-secret")
	t.Setenv("OIDC_REDIRECT_URL", "https://app.example.com/auth/callback")
	t.Setenv("OIDC_ROLES_CLAIM", "roles")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load with all OIDC vars: %v", err)
	}
	if c.OIDC.Issuer != "https://idp.example.com" {
		t.Errorf("OIDC.Issuer = %q", c.OIDC.Issuer)
	}
	if len(c.OIDC.Scopes) == 0 {
		t.Error("OIDC.Scopes must default to openid profile email")
	}
}

func TestOIDCRoleMap(t *testing.T) {
	setBase(t)
	t.Setenv("AUTH_MODE", "oidc")
	t.Setenv("OIDC_ISSUER", "https://idp.example.com")
	t.Setenv("OIDC_CLIENT_ID", "client-id")
	t.Setenv("OIDC_CLIENT_SECRET", "client-secret")
	t.Setenv("OIDC_REDIRECT_URL", "https://app.example.com/auth/callback")
	t.Setenv("OIDC_ROLES_CLAIM", "roles")
	t.Setenv("OIDC_ROLE_MAP", "admins:airllm_admin,devs:airllm_user")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.OIDC.RoleMap["admins"] != "airllm_admin" {
		t.Errorf("RoleMap[admins] = %q", c.OIDC.RoleMap["admins"])
	}
	if c.OIDC.RoleMap["devs"] != "airllm_user" {
		t.Errorf("RoleMap[devs] = %q", c.OIDC.RoleMap["devs"])
	}
}
