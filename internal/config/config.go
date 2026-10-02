// Package config loads and validates runtime configuration from the
// environment. It holds no secrets in source and fails fast on bad input.
package config

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/crypto/hkdf"
)

// OIDCConfig holds the OIDC relying-party configuration (mirrors auth.OIDCConfig
// to avoid an import cycle between config and auth).
type OIDCConfig struct {
	Issuer       string
	ClientID     string
	ClientSecret string
	RedirectURL  string
	Scopes       []string
	RolesClaim   string
	RoleMap      map[string]string
}

// Config is the validated runtime configuration.
type Config struct {
	HTTPAddr    string // listen address, e.g. ":8080"
	DatabaseURL string // postgres DSN (required)
	RedisURL    string // redis URL, e.g. "redis://host:6379/0"
	Env         string // "dev" | "prod"; used as the API-key environment tag
	AuthMode    string // "local" | "oidc"; "mock" is a deprecated alias for "local"

	MasterKey    []byte // 32-byte AES key for sealing provider credentials
	MasterKeyDev bool   // true when a deterministic dev key was derived (insecure)
	SessionKey   []byte // 32-byte HMAC key for signing session cookies

	OIDC OIDCConfig // populated when AuthMode == "oidc"
}

// Load reads configuration from the environment and validates it.
func Load() (*Config, error) {
	c := &Config{
		HTTPAddr:    env("HTTP_ADDR", ":8080"),
		DatabaseURL: os.Getenv("DATABASE_URL"),
		RedisURL:    env("REDIS_URL", "redis://localhost:6379/0"),
		Env:         env("ENV", "dev"),
		AuthMode:    env("AUTH_MODE", "local"),
	}

	if c.DatabaseURL == "" {
		return nil, fmt.Errorf("DATABASE_URL is required")
	}
	if c.Env != "dev" && c.Env != "prod" {
		return nil, fmt.Errorf("ENV must be \"dev\" or \"prod\", got %q", c.Env)
	}
	switch c.AuthMode {
	case "mock":
		c.AuthMode = "local" // deprecated alias
	case "local", "oidc":
		// ok
	default:
		return nil, fmt.Errorf("AUTH_MODE must be \"local\" or \"oidc\", got %q", c.AuthMode)
	}

	if c.AuthMode == "oidc" {
		oidcCfg, err := loadOIDC()
		if err != nil {
			return nil, err
		}
		c.OIDC = oidcCfg
	}

	key, dev, err := loadMasterKey(c.Env)
	if err != nil {
		return nil, err
	}
	c.MasterKey, c.MasterKeyDev = key, dev

	sk, err := loadSessionKey(c.MasterKey)
	if err != nil {
		return nil, err
	}
	c.SessionKey = sk

	return c, nil
}

// loadMasterKey reads AIRLLM_MASTER_KEY (base64, 32 bytes). In prod it is
// required; in dev a deterministic insecure key is derived so sealed
// credentials survive restarts without configuration.
func loadMasterKey(envName string) ([]byte, bool, error) {
	if v := os.Getenv("AIRLLM_MASTER_KEY"); v != "" {
		b, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			return nil, false, fmt.Errorf("AIRLLM_MASTER_KEY must be base64: %w", err)
		}
		if len(b) != 32 {
			return nil, false, fmt.Errorf("AIRLLM_MASTER_KEY must decode to 32 bytes, got %d", len(b))
		}
		return b, false, nil
	}
	if envName == "prod" {
		return nil, false, fmt.Errorf("AIRLLM_MASTER_KEY is required in prod")
	}
	sum := sha256.Sum256([]byte("airllm-dev-insecure-master-key"))
	return sum[:], true, nil
}

// devMasterKeySetting is the settings-table key under which a generated dev
// master key is persisted (see ResolveDevMasterKey).
const devMasterKeySetting = "dev_master_key"

type devMasterKeyPayload struct {
	KeyB64 string `json:"key_b64"`
}

// ResolveDevMasterKey replaces the dev placeholder master key — a fixed
// value derived from a hardcoded string, visible to anyone who reads this
// public repo's source, and therefore identical across every install that
// never sets AIRLLM_MASTER_KEY — with a random key generated once per
// install and persisted via putIfAbsent, so it stays constant across
// restarts and replicas of the SAME install without being a publicly
// knowable constant. No-op when MasterKeyDev is false (a real key was
// supplied via AIRLLM_MASTER_KEY). Must be called before anything uses
// MasterKey or SessionKey, once Postgres is reachable.
func (c *Config) ResolveDevMasterKey(ctx context.Context, putIfAbsent func(ctx context.Context, name string, value []byte) ([]byte, error)) error {
	if !c.MasterKeyDev {
		return nil
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return fmt.Errorf("generate dev master key: %w", err)
	}
	payload, err := json.Marshal(devMasterKeyPayload{KeyB64: base64.StdEncoding.EncodeToString(key)})
	if err != nil {
		return fmt.Errorf("encode dev master key: %w", err)
	}
	stored, err := putIfAbsent(ctx, devMasterKeySetting, payload)
	if err != nil {
		return fmt.Errorf("persist dev master key: %w", err)
	}
	var p devMasterKeyPayload
	if err := json.Unmarshal(stored, &p); err != nil {
		return fmt.Errorf("decode stored dev master key: %w", err)
	}
	resolved, err := base64.StdEncoding.DecodeString(p.KeyB64)
	if err != nil || len(resolved) != 32 {
		return fmt.Errorf("stored dev master key is invalid")
	}
	sk, err := loadSessionKey(resolved)
	if err != nil {
		return err
	}
	c.MasterKey = resolved
	c.SessionKey = sk
	return nil
}

// loadSessionKey returns the HMAC session signing key: AIRLLM_SESSION_KEY
// (base64, 32 bytes) when set, otherwise a deterministic key derived from the
// master key so sessions survive restarts and replicas without a new secret.
func loadSessionKey(master []byte) ([]byte, error) {
	if v := os.Getenv("AIRLLM_SESSION_KEY"); v != "" {
		b, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			return nil, fmt.Errorf("AIRLLM_SESSION_KEY must be base64: %w", err)
		}
		if len(b) != 32 {
			return nil, fmt.Errorf("AIRLLM_SESSION_KEY must decode to 32 bytes, got %d", len(b))
		}
		return b, nil
	}
	r := hkdf.New(sha256.New, master, nil, []byte("airllm-session-v1"))
	key := make([]byte, 32)
	if _, err := io.ReadFull(r, key); err != nil {
		return nil, fmt.Errorf("derive session key: %w", err)
	}
	return key, nil
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// loadOIDC reads and validates OIDC relying-party config from the environment.
func loadOIDC() (OIDCConfig, error) {
	required := []string{"OIDC_ISSUER", "OIDC_CLIENT_ID", "OIDC_CLIENT_SECRET", "OIDC_REDIRECT_URL", "OIDC_ROLES_CLAIM"}
	for _, k := range required {
		if os.Getenv(k) == "" {
			return OIDCConfig{}, fmt.Errorf("%s is required when AUTH_MODE=oidc", k)
		}
	}

	scopesRaw := env("OIDC_SCOPES", "openid profile email")
	scopes := strings.Fields(scopesRaw)

	return OIDCConfig{
		Issuer:       os.Getenv("OIDC_ISSUER"),
		ClientID:     os.Getenv("OIDC_CLIENT_ID"),
		ClientSecret: os.Getenv("OIDC_CLIENT_SECRET"),
		RedirectURL:  os.Getenv("OIDC_REDIRECT_URL"),
		Scopes:       scopes,
		RolesClaim:   os.Getenv("OIDC_ROLES_CLAIM"),
		RoleMap:      parseRoleMap(os.Getenv("OIDC_ROLE_MAP")),
	}, nil
}

// parseRoleMap parses a comma-separated "idp_role:airllm_role" mapping string.
func parseRoleMap(s string) map[string]string {
	out := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		if k, v, ok := strings.Cut(strings.TrimSpace(pair), ":"); ok && k != "" {
			out[k] = v
		}
	}
	return out
}
