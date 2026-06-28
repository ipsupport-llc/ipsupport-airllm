// Package config loads and validates runtime configuration from the
// environment. It holds no secrets in source and fails fast on bad input.
package config

import (
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

	return c, nil
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
