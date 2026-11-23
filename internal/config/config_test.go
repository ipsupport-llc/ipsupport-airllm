package config

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"
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
	t.Setenv("SEED_DEV", "")
	t.Setenv("TRUSTED_PROXY_CIDRS", "")
	t.Setenv("OIDC_ALLOW_HTTP_ISSUER", "")
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
	t.Setenv("LOOKUP_CACHE_TTL", "")
	t.Setenv("LOOKUP_CACHE_MAX_STALE", "")
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

func TestResolveDevMasterKeyNoOpWithRealKey(t *testing.T) {
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
	called := false
	if err := c.ResolveDevMasterKey(context.Background(), func(ctx context.Context, name string, value []byte) ([]byte, error) {
		called = true
		return value, nil
	}); err != nil {
		t.Fatalf("ResolveDevMasterKey: %v", err)
	}
	if called {
		t.Error("putIfAbsent must not be called when a real key was supplied")
	}
	if string(c.MasterKey) != string(raw) {
		t.Error("MasterKey must stay the explicitly configured value")
	}
}

func TestResolveDevMasterKeyPersistsAndRederivesSessionKey(t *testing.T) {
	setBase(t)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	placeholder := string(c.MasterKey)
	var persisted []byte
	if err := c.ResolveDevMasterKey(context.Background(), func(ctx context.Context, name string, value []byte) ([]byte, error) {
		persisted = value // simulate winning the race: store returns our own value
		return value, nil
	}); err != nil {
		t.Fatalf("ResolveDevMasterKey: %v", err)
	}
	if persisted == nil {
		t.Fatal("putIfAbsent was never called")
	}
	if len(c.MasterKey) != 32 {
		t.Fatalf("MasterKey length = %d, want 32", len(c.MasterKey))
	}
	if string(c.MasterKey) == placeholder {
		t.Error("MasterKey must no longer be the hardcoded dev placeholder")
	}
	wantSK, err := loadSessionKey(c.MasterKey)
	if err != nil {
		t.Fatalf("loadSessionKey: %v", err)
	}
	if string(c.SessionKey) != string(wantSK) {
		t.Error("SessionKey must be re-derived from the resolved master key")
	}
}

func TestResolveDevMasterKeyConvergesOnRaceLoser(t *testing.T) {
	setBase(t)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	existing := devMasterKeyPayload{KeyB64: base64.StdEncoding.EncodeToString(bytesOfLen32(7))}
	existingRaw, err := json.Marshal(existing)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	if err := c.ResolveDevMasterKey(context.Background(), func(ctx context.Context, name string, value []byte) ([]byte, error) {
		// Simulate losing the race: a concurrent writer already persisted
		// existingRaw, so a real PutSettingIfAbsent would hand that back
		// instead of our own generated value.
		return existingRaw, nil
	}); err != nil {
		t.Fatalf("ResolveDevMasterKey: %v", err)
	}
	wantKey, _ := base64.StdEncoding.DecodeString(existing.KeyB64)
	if string(c.MasterKey) != string(wantKey) {
		t.Error("MasterKey must converge on the value PutSettingIfAbsent actually returned, not the one generated locally")
	}
}

func bytesOfLen32(fill byte) []byte {
	b := make([]byte, 32)
	for i := range b {
		b[i] = fill
	}
	return b
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

// TestParseRoleMapLogsDuplicateKey is the Store/config Minor fix: a repeated
// idp_role key in OIDC_ROLE_MAP (an operator typo, e.g. "admin:x,admin:y")
// resolved last-wins via plain map assignment with zero validation or log
// line. Proves the last value still wins (unchanged behavior) AND that the
// collision is now logged, naming both the ignored and applied value.
func TestParseRoleMapLogsDuplicateKey(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	got := parseRoleMap("admin:airllm_admin,admin:airllm_user")
	if got["admin"] != "airllm_user" {
		t.Errorf("RoleMap[admin] = %q, want airllm_user (last one wins)", got["admin"])
	}
	logged := buf.String()
	if !strings.Contains(logged, "admin") || !strings.Contains(logged, "airllm_admin") || !strings.Contains(logged, "airllm_user") {
		t.Errorf("expected a warning naming the idp_role and both values, got log output: %s", logged)
	}
}

func TestLookupCacheDefaults(t *testing.T) {
	setBase(t)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.LookupCacheTTL != 30*time.Second || c.LookupCacheMaxStale != 5*time.Minute {
		t.Errorf("lookup cache = %v/%v, want 30s/5m", c.LookupCacheTTL, c.LookupCacheMaxStale)
	}
}

func TestLookupCacheFromEnv(t *testing.T) {
	setBase(t)
	t.Setenv("LOOKUP_CACHE_TTL", "10s")
	t.Setenv("LOOKUP_CACHE_MAX_STALE", "2m")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.LookupCacheTTL != 10*time.Second || c.LookupCacheMaxStale != 2*time.Minute {
		t.Errorf("lookup cache = %v/%v, want 10s/2m", c.LookupCacheTTL, c.LookupCacheMaxStale)
	}
}

func TestLookupCacheRejectsBadValues(t *testing.T) {
	for _, tc := range []struct{ ttl, stale string }{
		{"soon", ""},    // not a duration
		{"0s", ""},      // TTL must be positive
		{"1m", "30s"},   // MaxStale below TTL
		{"", "forever"}, // not a duration
	} {
		setBase(t)
		t.Setenv("LOOKUP_CACHE_TTL", tc.ttl)
		t.Setenv("LOOKUP_CACHE_MAX_STALE", tc.stale)
		if _, err := Load(); err == nil {
			t.Errorf("TTL=%q MAX_STALE=%q: want an error", tc.ttl, tc.stale)
		}
	}
}

func TestReplicaKnobDefaults(t *testing.T) {
	setBase(t)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.ConfigRefreshInterval != 10*time.Second {
		t.Errorf("config refresh = %v, want 10s", c.ConfigRefreshInterval)
	}
	if c.ShutdownDrain != 0 || c.ShutdownTimeout != 10*time.Second {
		t.Errorf("shutdown drain/timeout = %v/%v, want 0s/10s", c.ShutdownDrain, c.ShutdownTimeout)
	}
}

func TestReplicaKnobsFromEnv(t *testing.T) {
	setBase(t)
	t.Setenv("CONFIG_REFRESH_INTERVAL", "3s")
	t.Setenv("SHUTDOWN_DRAIN", "5s")
	t.Setenv("SHUTDOWN_TIMEOUT", "20s")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.ConfigRefreshInterval != 3*time.Second || c.ShutdownDrain != 5*time.Second || c.ShutdownTimeout != 20*time.Second {
		t.Errorf("refresh/drain/timeout = %v/%v/%v, want 3s/5s/20s", c.ConfigRefreshInterval, c.ShutdownDrain, c.ShutdownTimeout)
	}
}

func TestReplicaKnobsRejectBadValues(t *testing.T) {
	for _, tc := range []struct{ key, val string }{
		{"CONFIG_REFRESH_INTERVAL", "often"},
		{"CONFIG_REFRESH_INTERVAL", "0s"}, // must be positive
		{"SHUTDOWN_DRAIN", "-1s"},
		{"SHUTDOWN_DRAIN", "a bit"},
		{"SHUTDOWN_TIMEOUT", "0s"}, // must be positive
	} {
		setBase(t)
		t.Setenv(tc.key, tc.val)
		if _, err := Load(); err == nil {
			t.Errorf("%s=%q: want an error", tc.key, tc.val)
		}
	}
}

func TestUpstreamChatTimeoutDefault(t *testing.T) {
	setBase(t)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.UpstreamChatTimeout != 120*time.Second {
		t.Errorf("upstream chat timeout = %v, want 120s", c.UpstreamChatTimeout)
	}
}

func TestUpstreamChatTimeoutFromEnv(t *testing.T) {
	setBase(t)
	t.Setenv("UPSTREAM_CHAT_TIMEOUT", "5m")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.UpstreamChatTimeout != 5*time.Minute {
		t.Errorf("upstream chat timeout = %v, want 5m", c.UpstreamChatTimeout)
	}
}

func TestUpstreamChatTimeoutRejectsBadValues(t *testing.T) {
	for _, val := range []string{"often", "0s", "-30s"} {
		setBase(t)
		t.Setenv("UPSTREAM_CHAT_TIMEOUT", val)
		if _, err := Load(); err == nil {
			t.Errorf("UPSTREAM_CHAT_TIMEOUT=%q: want an error", val)
		}
	}
}

func TestForwardHeadersDefaultEmpty(t *testing.T) {
	setBase(t)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.ForwardHeaders) != 0 {
		t.Errorf("ForwardHeaders = %v, want empty (opt-in)", c.ForwardHeaders)
	}
}

func TestForwardHeadersRejectsDenylistedNames(t *testing.T) {
	for _, val := range []string{
		"Authorization", "authorization", "X-Api-Key", "Proxy-Authorization",
		"Cookie", "Host", "Content-Length", "Content-Type", "Content-Encoding",
		"Transfer-Encoding", "Connection", "Te", "Upgrade", "Accept-Encoding",
		"X-Request-Id,Authorization", // denied even mixed in with an allowed name
	} {
		setBase(t)
		t.Setenv("FORWARD_HEADERS", val)
		if _, err := Load(); err == nil {
			t.Errorf("FORWARD_HEADERS=%q: want an error (denylisted header name)", val)
		}
	}
}

func TestForwardHeadersFromEnv(t *testing.T) {
	setBase(t)
	t.Setenv("FORWARD_HEADERS", " X-Request-Id ,X-Session-Id,,X-Session-Id")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []string{"X-Request-Id", "X-Session-Id", "X-Session-Id"}
	if len(c.ForwardHeaders) != len(want) {
		t.Fatalf("ForwardHeaders = %v, want %v", c.ForwardHeaders, want)
	}
	for i, w := range want {
		if c.ForwardHeaders[i] != w {
			t.Errorf("ForwardHeaders[%d] = %q, want %q", i, c.ForwardHeaders[i], w)
		}
	}
}

// TestSeedDevIsExplicitOptIn proves ENV=dev alone never seeds the demo
// users/API key: SEED_DEV=1 is required, and it is refused outside
// ENV=dev + AUTH_MODE=local.
func TestSeedDevIsExplicitOptIn(t *testing.T) {
	setBase(t)
	t.Setenv("ENV", "dev")
	t.Setenv("AUTH_MODE", "local")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.SeedDev {
		t.Fatal("ENV=dev without SEED_DEV must not seed")
	}

	t.Setenv("SEED_DEV", "1")
	if c, err = Load(); err != nil || !c.SeedDev {
		t.Fatalf("SEED_DEV=1 with ENV=dev+local: SeedDev=%v err=%v", c != nil && c.SeedDev, err)
	}
	t.Setenv("AUTH_MODE", "mock")
	if c, err = Load(); err != nil || !c.SeedDev {
		t.Fatalf("SEED_DEV=1 with the mock alias: err=%v", err)
	}

	t.Setenv("AUTH_MODE", "local")
	t.Setenv("ENV", "prod")
	t.Setenv("AIRLLM_MASTER_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if _, err := Load(); err == nil {
		t.Fatal("SEED_DEV=1 with ENV=prod must error")
	}

	t.Setenv("ENV", "dev")
	t.Setenv("SEED_DEV", "yes")
	if _, err := Load(); err == nil {
		t.Fatal("SEED_DEV must be 0 or 1")
	}
}

func TestOIDCIssuerRequiresHTTPS(t *testing.T) {
	cases := []struct {
		env, issuer string
		ok          bool
	}{
		{"prod", "https://idp.example.com", true},
		{"prod", "http://idp.example.com", false},
		{"dev", "http://idp.example.com", false},
		{"prod", "http://localhost:5556", false},
		{"dev", "http://localhost:5556", true},
		{"dev", "http://127.0.0.1:5556/dex", true},
		{"dev", "idp.example.com", false},
	}
	for _, tc := range cases {
		setBase(t)
		t.Setenv("ENV", tc.env)
		t.Setenv("AIRLLM_MASTER_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
		t.Setenv("AUTH_MODE", "oidc")
		t.Setenv("OIDC_ISSUER", tc.issuer)
		t.Setenv("OIDC_CLIENT_ID", "client-id")
		t.Setenv("OIDC_CLIENT_SECRET", "client-secret")
		t.Setenv("OIDC_REDIRECT_URL", "https://app.example.com/auth/callback")
		t.Setenv("OIDC_ROLES_CLAIM", "roles")
		_, err := Load()
		if (err == nil) != tc.ok {
			t.Errorf("ENV=%s OIDC_ISSUER=%q: err=%v, want ok=%v", tc.env, tc.issuer, err, tc.ok)
		}
	}
}

func TestTrustedProxyCIDRs(t *testing.T) {
	setBase(t)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.TrustedProxyCIDRs) != len(DefaultTrustedProxyCIDRs) {
		t.Fatalf("default = %v", c.TrustedProxyCIDRs)
	}

	t.Setenv("TRUSTED_PROXY_CIDRS", " 10.42.0.0/16, fd00::/8 ,")
	if c, err = Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.TrustedProxyCIDRs) != 2 || c.TrustedProxyCIDRs[0].String() != "10.42.0.0/16" {
		t.Fatalf("parsed = %v", c.TrustedProxyCIDRs)
	}

	t.Setenv("TRUSTED_PROXY_CIDRS", "10.42.0.1")
	if _, err := Load(); err == nil {
		t.Fatal("a bare address must be rejected")
	}
}

// TestOIDCAllowHTTPIssuerEscapeHatch proves OIDC_ALLOW_HTTP_ISSUER=1 admits
// an existing internal http issuer in prod, and logs a warning for it.
func TestOIDCAllowHTTPIssuerEscapeHatch(t *testing.T) {
	setBase(t)
	t.Setenv("ENV", "prod")
	t.Setenv("AIRLLM_MASTER_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	t.Setenv("AUTH_MODE", "oidc")
	t.Setenv("OIDC_ISSUER", "http://dex.idp.svc:5556/dex")
	t.Setenv("OIDC_CLIENT_ID", "client-id")
	t.Setenv("OIDC_CLIENT_SECRET", "client-secret")
	t.Setenv("OIDC_REDIRECT_URL", "https://app.example.com/auth/callback")
	t.Setenv("OIDC_ROLES_CLAIM", "roles")
	if _, err := Load(); err == nil {
		t.Fatal("http issuer must be refused without the escape hatch")
	}

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	t.Setenv("OIDC_ALLOW_HTTP_ISSUER", "1")
	if _, err := Load(); err != nil {
		t.Fatalf("OIDC_ALLOW_HTTP_ISSUER=1 must admit the http issuer: %v", err)
	}
	if !strings.Contains(buf.String(), "OIDC_ALLOW_HTTP_ISSUER") || !strings.Contains(buf.String(), "level=WARN") {
		t.Fatalf("expected a WARN log, got %q", buf.String())
	}
}
