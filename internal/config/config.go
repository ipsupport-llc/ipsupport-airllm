// Package config loads and validates runtime configuration from the
// environment. It holds no secrets in source and fails fast on bad input.
package config

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"os"
)

// Config is the validated runtime configuration.
type Config struct {
	HTTPAddr    string // listen address, e.g. ":8080"
	DatabaseURL string // postgres DSN (required)
	RedisURL    string // redis URL, e.g. "redis://host:6379/0"
	Env         string // "dev" | "prod"; used as the API-key environment tag
	AuthMode    string // "mock" | "oidc"; "mock" skips OIDC for local use

	MasterKey    []byte // 32-byte AES key for sealing provider credentials
	MasterKeyDev bool   // true when a deterministic dev key was derived (insecure)
}

// Load reads configuration from the environment and validates it.
func Load() (*Config, error) {
	c := &Config{
		HTTPAddr:    env("HTTP_ADDR", ":8080"),
		DatabaseURL: os.Getenv("DATABASE_URL"),
		RedisURL:    env("REDIS_URL", "redis://localhost:6379/0"),
		Env:         env("ENV", "dev"),
		AuthMode:    env("AUTH_MODE", "mock"),
	}

	if c.DatabaseURL == "" {
		return nil, fmt.Errorf("DATABASE_URL is required")
	}
	if c.Env != "dev" && c.Env != "prod" {
		return nil, fmt.Errorf("ENV must be \"dev\" or \"prod\", got %q", c.Env)
	}
	if c.AuthMode != "mock" && c.AuthMode != "oidc" {
		return nil, fmt.Errorf("AUTH_MODE must be \"mock\" or \"oidc\", got %q", c.AuthMode)
	}

	key, dev, err := loadMasterKey(c.Env)
	if err != nil {
		return nil, err
	}
	c.MasterKey, c.MasterKeyDev = key, dev

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

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
