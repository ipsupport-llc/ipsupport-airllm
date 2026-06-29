# Auth Foundation (P1) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the random-per-boot mock authenticator with production-grade control-plane auth: a full local mode (persistent DB users, admin CRUD, self password change, stable sessions, idempotent bootstrap admin) plus a full generic OIDC mode.

**Architecture:** A shared HMAC session codec (keyed by a stable key derived from the master key) underlies two interchangeable providers — `LocalAuth` (bcrypt over the `users` table) and `OIDCAuth` (go-oidc + oauth2, PKCE). The rest of the app stays auth-method-agnostic behind the existing `auth.Authenticator` / `auth.LoginProvider` / `auth.Principal` abstraction. Auth-logic is unit-tested with fake stores / a mock IdP; the Postgres `UserStore` impl and the HTTP endpoints are verified live against the compose stack (the project has no DB unit harness — same pattern as the capture store).

**Tech Stack:** Go 1.26, `golang.org/x/crypto/bcrypt`, `golang.org/x/crypto/hkdf`, `github.com/coreos/go-oidc/v3/oidc`, `golang.org/x/oauth2`, `jackc/pgx/v5`.

## Global Constraints

- Module path is `github.com/ipsupport-llc/ipsupport-airllm`.
- `AUTH_MODE` accepts `local | oidc | mock`; `mock` is a deprecated alias normalized to `local`.
- Session key: `AIRLLM_SESSION_KEY` (base64, 32 bytes) if set, else `HKDF-SHA256(masterKey, salt=nil, info="airllm-session-v1", 32)`. Never logged.
- Passwords: bcrypt at `bcrypt.DefaultCost`; minimum length 8.
- Bootstrap admin password: `AIRLLM_ADMIN_PASSWORD` if set (never logged), else a random token logged exactly once at WARN. Username `AIRLLM_ADMIN_USERNAME` (default `admin`).
- Public-clean: no issuer URLs, client IDs/secrets, hostnames, IPs, or passwords in the repo — env vars only, placeholders in docs.
- English-only repo. `go test -race ./...`, `go vet`, `gofmt -l` must stay clean.
- Commit trailer: `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`.
- Session cookies: `HttpOnly`, `SameSite=Lax`, 12h TTL (existing `sessionTTL`).

## File Structure

- `internal/auth/session.go` (create) — `Session` HMAC codec (sign/verify/cookie), extracted so both providers share it.
- `internal/auth/password.go` (create) — bcrypt `HashPassword` / `CheckPassword` + timing dummy.
- `internal/auth/store.go` (create) — `UserStore` interface + `UserRow` (consumer-defined; implemented in `internal/store`).
- `internal/auth/local.go` (create) — `LocalAuth` provider + `EnsureBootstrapAdmin`.
- `internal/auth/oidc.go` (create) — `OIDCAuth` provider + login/callback + roles parsing/mapping.
- `internal/auth/auth.go` (modify) — keep `Principal`/interfaces/`Roles`/`payload`; remove `Mock`.
- `internal/config/config.go` (modify) — `SessionKey`, `AUTH_MODE` values, OIDC vars + validation.
- `internal/store/users.go` (create) — `PGUsers` implementing `auth.UserStore`.
- `internal/httpapi/api_users.go` (create) — admin user CRUD, `/api/me/password`, `/api/auth/mode`.
- `internal/httpapi/api_admin.go` (modify) — register user CRUD routes; extend `handleAdminUsers`.
- `internal/httpapi/api_self.go` (modify) — register `/api/me/password`.
- `internal/httpapi/server.go` (modify) — route registration for `/auth/sso`, `/auth/callback`, `/api/auth/mode`.
- `cmd/ipsupport-airllm/main.go` (modify) — provider selection by `AUTH_MODE`.
- `internal/seed/seed.go` (modify) — persist dev demo user passwords.
- `migrations/0007_local_auth.sql` (create).
- `web/static/app.js` (modify) — users CRUD, change-password, SSO button.
- Docs: `docs/configuration.md`, `docs/api.md`, `docs/getting-started.md`, `docs/operations.md` (modify).

---

### Task 1: Shared session codec + stable session key

**Files:**
- Create: `internal/auth/session.go`
- Modify: `internal/config/config.go`
- Test: `internal/auth/session_test.go`, `internal/config/config_test.go`

**Interfaces:**
- Produces: `auth.NewSession(key []byte) *Session`; `(*Session).Sign(p Principal) string`; `(*Session).SetSession(w, p)`; `(*Session).ClearSession(w)`; `(*Session).Authenticate(r) (Principal, error)`. `config.Config.SessionKey []byte`.

- [ ] **Step 1: Write the failing test** — `internal/auth/session_test.go`

```go
package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSessionRoundTrip(t *testing.T) {
	s := NewSession([]byte("0123456789abcdef0123456789abcdef"))
	p := Principal{Subject: "admin", Email: "a@b", Roles: []string{AdminRole}}

	rec := httptest.NewRecorder()
	s.SetSession(rec, p)
	cookie := rec.Result().Cookies()[0]

	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(cookie)
	got, err := s.Authenticate(req)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if got.Subject != "admin" || !got.IsAdmin() {
		t.Fatalf("principal mismatch: %+v", got)
	}
}

func TestSessionCrossInstanceSameKey(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	rec := httptest.NewRecorder()
	NewSession(key).SetSession(rec, Principal{Subject: "x"})
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(rec.Result().Cookies()[0])
	if _, err := NewSession(key).Authenticate(req); err != nil {
		t.Fatalf("a cookie signed by one instance must verify on another with the same key: %v", err)
	}
}

func TestSessionRejectsTamperAndWrongKey(t *testing.T) {
	rec := httptest.NewRecorder()
	NewSession([]byte("0123456789abcdef0123456789abcdef")).SetSession(rec, Principal{Subject: "x"})
	c := rec.Result().Cookies()[0]
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(c)
	if _, err := NewSession([]byte("DIFFERENTdef0123456789abcdef0123")).Authenticate(req); err != ErrNoSession {
		t.Fatal("a different key must reject the cookie")
	}
	bad := &http.Cookie{Name: cookieName, Value: c.Value + "x"}
	req2 := httptest.NewRequest("GET", "/", nil)
	req2.AddCookie(bad)
	if _, err := NewSession([]byte("0123456789abcdef0123456789abcdef")).Authenticate(req2); err != ErrNoSession {
		t.Fatal("a tampered cookie must reject")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/auth/ -run TestSession`
Expected: FAIL (`NewSession` undefined).

- [ ] **Step 3: Create `internal/auth/session.go`** (move the codec out of `Mock`)

```go
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// Session is the HMAC-signed cookie codec shared by every auth provider. The
// signing key must be stable across restarts and replicas (see config).
type Session struct {
	key []byte
}

// NewSession returns a session codec keyed by key.
func NewSession(key []byte) *Session { return &Session{key: key} }

// SetSession writes a signed session cookie for the principal.
func (s *Session) SetSession(w http.ResponseWriter, p Principal) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    s.sign(payload{Sub: p.Subject, Email: p.Email, Roles: p.Roles, Exp: time.Now().Add(sessionTTL).Unix()}),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionTTL.Seconds()),
	})
}

// ClearSession expires the session cookie.
func (s *Session) ClearSession(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", HttpOnly: true, MaxAge: -1})
}

// Authenticate validates the session cookie and returns its principal.
func (s *Session) Authenticate(r *http.Request) (Principal, error) {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return Principal{}, ErrNoSession
	}
	body, sig, ok := strings.Cut(c.Value, ".")
	if !ok || !hmac.Equal([]byte(sig), []byte(s.macOf(body))) {
		return Principal{}, ErrNoSession
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return Principal{}, ErrNoSession
	}
	var p payload
	if err := json.Unmarshal(raw, &p); err != nil {
		return Principal{}, ErrNoSession
	}
	if time.Now().Unix() > p.Exp {
		return Principal{}, ErrNoSession
	}
	return Principal{Subject: p.Sub, Email: p.Email, Roles: p.Roles}, nil
}

func (s *Session) sign(p payload) string {
	b, _ := json.Marshal(p)
	body := base64.RawURLEncoding.EncodeToString(b)
	return body + "." + s.macOf(body)
}

func (s *Session) macOf(body string) string {
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(body))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
```

This task is **pure addition**: `session.go` is new, and `Mock` in `auth.go` is left untouched (it keeps its own duplicate codec for now). The shared `payload`/`cookieName`/`sessionTTL`/`ErrNoSession` already live in `auth.go` and are reused by `Session`. `Mock` is deleted in Task 3 — keeping it here means the package stays green with no cascade.

- [ ] **Step 4: Add the session key to config** — `internal/config/config.go`

Add `SessionKey []byte` to `Config`. After `c.MasterKey, c.MasterKeyDev = key, dev` in `Load`, add:

```go
sk, err := loadSessionKey(c.MasterKey)
if err != nil {
	return nil, err
}
c.SessionKey = sk
```

Add (imports: `crypto/sha256`, `io`, `golang.org/x/crypto/hkdf`):

```go
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
```

Add to `internal/config/config_test.go`:

```go
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
```

Also add `t.Setenv("AIRLLM_SESSION_KEY", "")` to the existing `setBase` helper so other config tests are isolated.

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/auth/ ./internal/config/ && go vet ./internal/auth/ ./internal/config/`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/auth/session.go internal/config/config.go internal/auth/session_test.go internal/config/config_test.go go.mod go.sum
git commit -m "feat(auth): shared session codec + stable HKDF session key"
```

---

### Task 2: bcrypt helpers + users store + migration 0007

**Files:**
- Create: `internal/auth/password.go`, `internal/auth/store.go`, `internal/store/users.go`, `migrations/0007_local_auth.sql`
- Test: `internal/auth/password_test.go`

**Interfaces:**
- Produces: `auth.HashPassword(string) (string, error)`; `auth.CheckPassword(hash, pw string) bool`; `auth.UserRow{ID,Subject,Email,Display string; Roles []string; PasswordHash string; Disabled bool; AuthSource string}`; the `auth.UserStore` interface; `store.NewPGUsers(*store.Store) *store.PGUsers` (implements `auth.UserStore`).

- [ ] **Step 1: Write the failing test** — `internal/auth/password_test.go`

```go
package auth

import "testing"

func TestHashAndCheckPassword(t *testing.T) {
	h, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !CheckPassword(h, "correct horse battery") {
		t.Error("correct password must verify")
	}
	if CheckPassword(h, "wrong") {
		t.Error("wrong password must not verify")
	}
	if CheckPassword("", "anything") {
		t.Error("empty hash must never verify (no local password)")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/auth/ -run TestHashAndCheck`
Expected: FAIL (`HashPassword` undefined).

- [ ] **Step 3: Create `internal/auth/password.go`** (import `golang.org/x/crypto/bcrypt`)

```go
package auth

import "golang.org/x/crypto/bcrypt"

// dummyHash is a valid bcrypt hash of a random value, compared against on
// "user not found" so login timing does not reveal whether a user exists.
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("airllm-timing-dummy"), bcrypt.DefaultCost)

// HashPassword returns a bcrypt hash of pw.
func HashPassword(pw string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	return string(b), err
}

// CheckPassword reports whether pw matches the bcrypt hash. An empty hash
// (no local password set) never matches.
func CheckPassword(hash, pw string) bool {
	if hash == "" {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
}

// checkAgainstDummy burns equivalent time on the not-found path.
func checkAgainstDummy(pw string) { _ = bcrypt.CompareHashAndPassword(dummyHash, []byte(pw)) }
```

- [ ] **Step 4: Create `internal/auth/store.go`** (the consumer-defined interface)

```go
package auth

import "context"

// UserRow is a control-plane user record.
type UserRow struct {
	ID           string
	Subject      string
	Email        string
	Display      string
	Roles        []string
	PasswordHash string
	Disabled     bool
	AuthSource   string // "local" | "oidc"
}

// UserStore is the persistence the auth providers need. Implemented by
// internal/store.PGUsers.
type UserStore interface {
	ByUsername(ctx context.Context, username string) (UserRow, bool, error) // match subject (ci) or email
	CountAdmins(ctx context.Context) (int, error)
	CreateLocal(ctx context.Context, u UserRow) (string, error) // returns id
}
```

- [ ] **Step 5: Create `migrations/0007_local_auth.sql`**

```sql
-- Local auth: persistent password login over the users table.
ALTER TABLE users ADD COLUMN IF NOT EXISTS password_hash   text NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN IF NOT EXISTS password_set_at timestamptz NULL;
ALTER TABLE users ADD COLUMN IF NOT EXISTS disabled        bool NOT NULL DEFAULT false;
ALTER TABLE users ADD COLUMN IF NOT EXISTS auth_source     text NOT NULL DEFAULT 'local';
```

- [ ] **Step 6: Create `internal/store/users.go`** (PG impl; live-verified, no unit test)

```go
package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/auth"
)

// PGUsers implements auth.UserStore and the admin user-CRUD queries.
type PGUsers struct{ st *Store }

func NewPGUsers(st *Store) *PGUsers { return &PGUsers{st: st} }

func (p *PGUsers) ByUsername(ctx context.Context, username string) (auth.UserRow, bool, error) {
	var u auth.UserRow
	err := p.st.PG.QueryRow(ctx, `
		SELECT id::text, subject, email, display, roles, password_hash, disabled, auth_source
		FROM users WHERE lower(subject) = lower($1) OR (email <> '' AND lower(email) = lower($1))
		ORDER BY (lower(subject) = lower($1)) DESC LIMIT 1`, username,
	).Scan(&u.ID, &u.Subject, &u.Email, &u.Display, &u.Roles, &u.PasswordHash, &u.Disabled, &u.AuthSource)
	if errors.Is(err, pgx.ErrNoRows) {
		return auth.UserRow{}, false, nil
	}
	return u, err == nil, err
}

func (p *PGUsers) CountAdmins(ctx context.Context) (int, error) {
	var n int
	err := p.st.PG.QueryRow(ctx,
		`SELECT count(*) FROM users WHERE NOT disabled AND 'airllm_admin' = ANY(roles)`).Scan(&n)
	return n, err
}

func (p *PGUsers) CreateLocal(ctx context.Context, u auth.UserRow) (string, error) {
	var id string
	err := p.st.PG.QueryRow(ctx, `
		INSERT INTO users (subject, email, display, roles, password_hash, password_set_at, disabled, auth_source)
		VALUES ($1, $2, $3, $4, $5, now(), $6, 'local')
		RETURNING id::text`,
		u.Subject, u.Email, u.Display, u.Roles, u.PasswordHash, u.Disabled,
	).Scan(&id)
	return id, err
}
```

> The remaining admin-CRUD methods (`Update`, `SetPassword`, `SetDisabled`, `Delete`, `List`) are added in Task 5 alongside their handlers.

- [ ] **Step 7: Build + test**

Run: `go build ./... && go test ./internal/auth/ -run TestHashAndCheck && go vet ./...`
Expected: build OK, PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/auth/password.go internal/auth/store.go internal/store/users.go migrations/0007_local_auth.sql internal/auth/password_test.go go.mod go.sum
git commit -m "feat(auth): bcrypt helpers, UserStore interface, users migration"
```

---

### Task 3: LocalAuth provider + bootstrap admin

**Files:**
- Create: `internal/auth/local.go`
- Modify: `internal/auth/auth.go` (remove `Mock`, `mockUser`, `NewMock`), `internal/auth/auth_test.go` (drop Mock tests)
- Test: `internal/auth/local_test.go`

**Interfaces:**
- Consumes: `Session` (Task 1), `CheckPassword`/`HashPassword`/`UserStore`/`UserRow` (Task 2).
- Produces: `NewLocalAuth(store UserStore, sess *Session) *LocalAuth` (implements `LoginProvider` + `Authenticator`); `EnsureBootstrapAdmin(ctx, store UserStore, username, envPassword string) (created bool, generated string, err error)`.

- [ ] **Step 1: Write the failing test** — `internal/auth/local_test.go`

```go
package auth

import (
	"context"
	"testing"
)

type fakeUsers struct {
	byName  map[string]UserRow
	admins  int
	created []UserRow
}

func (f *fakeUsers) ByUsername(_ context.Context, u string) (UserRow, bool, error) {
	r, ok := f.byName[u]
	return r, ok, nil
}
func (f *fakeUsers) CountAdmins(_ context.Context) (int, error) { return f.admins, nil }
func (f *fakeUsers) CreateLocal(_ context.Context, u UserRow) (string, error) {
	f.created = append(f.created, u)
	return "new-id", nil
}

func newLocal(t *testing.T, users map[string]UserRow) *LocalAuth {
	t.Helper()
	return NewLocalAuth(&fakeUsers{byName: users}, NewSession([]byte("0123456789abcdef0123456789abcdef")))
}

func TestLocalLoginSuccess(t *testing.T) {
	h, _ := HashPassword("pw12345678")
	la := newLocal(t, map[string]UserRow{"admin": {Subject: "admin", Roles: []string{AdminRole}, PasswordHash: h}})
	p, ok := la.Login("admin", "pw12345678")
	if !ok || !p.IsAdmin() {
		t.Fatalf("admin login should succeed with admin role, ok=%v p=%+v", ok, p)
	}
}

func TestLocalLoginRejectsDisabledWrongUnknown(t *testing.T) {
	h, _ := HashPassword("pw12345678")
	la := newLocal(t, map[string]UserRow{
		"admin": {Subject: "admin", PasswordHash: h},
		"off":   {Subject: "off", PasswordHash: h, Disabled: true},
	})
	if _, ok := la.Login("admin", "wrong"); ok {
		t.Error("wrong password must fail")
	}
	if _, ok := la.Login("off", "pw12345678"); ok {
		t.Error("disabled user must fail")
	}
	if _, ok := la.Login("ghost", "pw12345678"); ok {
		t.Error("unknown user must fail")
	}
}

func TestEnsureBootstrapAdmin(t *testing.T) {
	// no admins -> create with env password, not logged/returned
	f := &fakeUsers{byName: map[string]UserRow{}, admins: 0}
	created, gen, err := EnsureBootstrapAdmin(context.Background(), f, "admin", "envsecret")
	if err != nil || !created || gen != "" || len(f.created) != 1 {
		t.Fatalf("env bootstrap: created=%v gen=%q err=%v n=%d", created, gen, err, len(f.created))
	}
	if !CheckPassword(f.created[0].PasswordHash, "envsecret") {
		t.Error("bootstrap admin must use the env password")
	}
	// admins already exist -> no-op
	f2 := &fakeUsers{admins: 1}
	created2, _, _ := EnsureBootstrapAdmin(context.Background(), f2, "admin", "")
	if created2 || len(f2.created) != 0 {
		t.Error("existing admin must skip bootstrap")
	}
	// no env password -> generate and return it (caller logs once)
	f3 := &fakeUsers{byName: map[string]UserRow{}}
	_, gen3, _ := EnsureBootstrapAdmin(context.Background(), f3, "admin", "")
	if gen3 == "" || !CheckPassword(f3.created[0].PasswordHash, gen3) {
		t.Error("generated bootstrap password must be returned and match the hash")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/auth/ -run 'TestLocal|TestEnsureBootstrap'`
Expected: FAIL (`NewLocalAuth` undefined).

- [ ] **Step 3: Create `internal/auth/local.go`**

```go
package auth

import "context"

// LocalAuth authenticates against bcrypt password hashes in the users table.
type LocalAuth struct {
	store UserStore
	*Session
}

// NewLocalAuth builds a local password authenticator.
func NewLocalAuth(store UserStore, sess *Session) *LocalAuth {
	return &LocalAuth{store: store, Session: sess}
}

// Login validates username/password against the store.
func (l *LocalAuth) Login(username, password string) (Principal, bool) {
	u, ok, err := l.store.ByUsername(context.Background(), username)
	if err != nil || !ok {
		checkAgainstDummy(password) // constant-ish time on not-found
		return Principal{}, false
	}
	if u.Disabled || !CheckPassword(u.PasswordHash, password) {
		return Principal{}, false
	}
	return Principal{Subject: u.Subject, Email: u.Email, Roles: u.Roles}, true
}

// The embedded *Session promotes SetSession/ClearSession/Authenticate, so
// LocalAuth satisfies both interfaces:
var _ LoginProvider = (*LocalAuth)(nil)
var _ Authenticator = (*LocalAuth)(nil)

// EnsureBootstrapAdmin creates an admin user when none exists. If envPassword
// is set it is used (and never returned); otherwise a random password is
// generated and returned so the caller can log it exactly once.
func EnsureBootstrapAdmin(ctx context.Context, store UserStore, username, envPassword string) (created bool, generated string, err error) {
	n, err := store.CountAdmins(ctx)
	if err != nil {
		return false, "", err
	}
	if n > 0 {
		return false, "", nil
	}
	pw := envPassword
	if pw == "" {
		pw = randToken(18)
		generated = pw
	}
	hash, err := HashPassword(pw)
	if err != nil {
		return false, "", err
	}
	if _, err := store.CreateLocal(ctx, UserRow{
		Subject: username, Email: username + "@local", Display: username,
		Roles: []string{AdminRole}, PasswordHash: hash,
	}); err != nil {
		return false, "", err
	}
	return true, generated, nil
}
```

> Delete the `Mock`/`mockUser`/`NewMock` block and the `*Mock` codec methods from `internal/auth/auth.go`. Remove the obsolete Mock tests from `internal/auth/auth_test.go` (keep the `Principal`/role tests). The package must build.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/auth/ && go vet ./internal/auth/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/auth/local.go internal/auth/auth.go internal/auth/auth_test.go internal/auth/local_test.go
git commit -m "feat(auth): LocalAuth provider + idempotent bootstrap admin"
```

---

### Task 4: Wire local mode + AUTH_MODE + /api/auth/mode + dev seed passwords

**Files:**
- Modify: `internal/config/config.go`, `cmd/ipsupport-airllm/main.go`, `internal/seed/seed.go`, `internal/httpapi/api_self.go`, `internal/httpapi/server.go`
- Test: `internal/config/config_test.go`

**Interfaces:**
- Consumes: `NewLocalAuth`, `EnsureBootstrapAdmin`, `NewSession`, `store.NewPGUsers`.
- Produces: working password login that persists across restarts; `GET /api/auth/mode` → `{"mode": "...", "sso_url": "..."}`.

- [ ] **Step 1: Write the failing test** — add to `internal/config/config_test.go`

```go
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
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/config/ -run TestAuthMode`
Expected: FAIL (mock not normalized / unknown accepted).

- [ ] **Step 3: Update `config.go` AUTH_MODE handling**

Replace the existing `AUTH_MODE` validation block in `Load`:

```go
switch c.AuthMode {
case "mock":
	c.AuthMode = "local" // deprecated alias
case "local", "oidc":
	// ok
default:
	return nil, fmt.Errorf("AUTH_MODE must be \"local\" or \"oidc\", got %q", c.AuthMode)
}
```

- [ ] **Step 4: Wire local mode in `main.go`**

Replace the `if cfg.AuthMode == "mock" { ... }` block with:

```go
session := auth.NewSession(cfg.SessionKey)
switch cfg.AuthMode {
case "local":
	users := store.NewPGUsers(st)
	la := auth.NewLocalAuth(users, session)
	deps.Auth = la
	deps.Login = la
	if cfg.AuthMode == "mock" {
		slog.Warn("AUTH_MODE=mock is a deprecated alias for local")
	}
	created, gen, err := auth.EnsureBootstrapAdmin(ctx, users,
		envOr("AIRLLM_ADMIN_USERNAME", "admin"), os.Getenv("AIRLLM_ADMIN_PASSWORD"))
	if err != nil {
		return fmt.Errorf("bootstrap admin: %w", err)
	}
	if created && gen != "" {
		slog.Warn("bootstrap admin created (change this password)", "username", envOr("AIRLLM_ADMIN_USERNAME", "admin"), "password", gen)
	}
case "oidc":
	// wired in Task 6
}
```

Add the small helper near the top of `main.go` if not present:

```go
func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
```

(Ensure `os`, `fmt`, and the `auth`/`store` imports are present.) The existing dev-seed call (`if cfg.Env == "dev" && cfg.AuthMode == "mock"`) must change its condition to `cfg.AuthMode == "local"`.

- [ ] **Step 5: Persist dev demo passwords in `internal/seed/seed.go`**

Add a dev-only constant and set passwords on the demo users (idempotent — only sets when empty). After the existing `dev-auditor` insert, add an `operator` demo user and stamp passwords:

```go
// DevPassword is the fixed password for the seeded demo users (operator,
// auditor, dev-admin). DEV ONLY — never used outside local development.
const DevPassword = "devpass123"
```

Then, after the demo users are inserted, stamp the hash where missing:

```go
devHash, err := auth.HashPassword(DevPassword)
if err != nil {
	return "", fmt.Errorf("seed password hash: %w", err)
}
if _, err := st.PG.Exec(ctx, `
	INSERT INTO users (subject, email, display, roles, password_hash, password_set_at)
	VALUES ('operator', 'operator@local', 'Dev Operator', ARRAY['airllm_user'], $1, now())
	ON CONFLICT (subject) DO NOTHING`, devHash); err != nil {
	return "", fmt.Errorf("seed operator user: %w", err)
}
if _, err := st.PG.Exec(ctx, `
	UPDATE users SET password_hash = $1, password_set_at = now()
	WHERE subject IN ('dev-admin','dev-auditor','operator') AND password_hash = ''`, devHash); err != nil {
	return "", fmt.Errorf("seed demo passwords: %w", err)
}
```

(Add the `auth` import to `seed.go`.)

- [ ] **Step 6: Add `/api/auth/mode`** — `internal/httpapi/api_self.go`

```go
// handleAuthMode reports the active auth mode so the login screen can render
// either the password form (local) or an SSO button (oidc).
func (s *Server) handleAuthMode(w http.ResponseWriter, _ *http.Request) {
	mode := "local"
	if s.login == nil {
		mode = "oidc"
	}
	resp := map[string]string{"mode": mode}
	if mode == "oidc" {
		resp["sso_url"] = "/auth/sso"
	}
	writeJSON(w, http.StatusOK, resp)
}
```

Register it in `server.go` next to `/auth/login` (public, no auth):

```go
s.mux.HandleFunc("GET /api/auth/mode", s.handleAuthMode)
```

- [ ] **Step 7: Build + race test + live-verify**

Run: `go build ./... && go test -race ./... && go vet ./...`
Then live-verify persistence (compose):

```bash
make compose-up   # or: docker compose -f deploy/docker-compose.yml up -d --build app
# capture the bootstrap admin password from the logs (logged once)
docker compose -f deploy/docker-compose.yml logs app | grep "bootstrap admin created"
# log in, then RESTART, then log in again with the SAME password -> still 200
```

Expected: build/tests pass; the bootstrap admin password is identical before and after a restart (no longer random-per-boot).

- [ ] **Step 8: Commit**

```bash
git add internal/config/config.go cmd/ipsupport-airllm/main.go internal/seed/seed.go internal/httpapi/api_self.go internal/httpapi/server.go internal/config/config_test.go
git commit -m "feat(auth): wire local mode, persistent bootstrap admin, /api/auth/mode"
```

---

### Task 5: User CRUD (admin) + self password change

**Files:**
- Create: `internal/httpapi/api_users.go`
- Modify: `internal/store/users.go` (add CRUD methods), `internal/httpapi/api_admin.go` (routes + extend list), `internal/httpapi/api_self.go` (route)
- Test: `internal/httpapi/api_users_test.go`

**Interfaces:**
- Consumes: `store.PGUsers`, `auth.HashPassword`/`CheckPassword`.
- Produces: `POST/PUT/DELETE /api/admin/users[...]`, `POST /api/admin/users/{id}/password`, `POST /api/me/password`. Pure helpers `validateNewUser(...)` and `validatePasswordChange(...)` for unit tests.

- [ ] **Step 1: Write the failing test** — `internal/httpapi/api_users_test.go`

```go
package httpapi

import "testing"

func TestValidateNewUser(t *testing.T) {
	known := map[string]bool{"airllm_admin": true, "airllm_user": true}
	if err := validateNewUser("alice", []string{"airllm_user"}, "longenough", known); err != nil {
		t.Errorf("valid user rejected: %v", err)
	}
	if err := validateNewUser("", []string{"airllm_user"}, "longenough", known); err == nil {
		t.Error("empty username must fail")
	}
	if err := validateNewUser("bob", []string{"airllm_user"}, "short", known); err == nil {
		t.Error("short password must fail")
	}
	if err := validateNewUser("bob", []string{"nope"}, "longenough", known); err == nil {
		t.Error("unknown role must fail")
	}
}

func TestValidatePasswordLen(t *testing.T) {
	if err := validatePassword("1234567"); err == nil {
		t.Error("7-char password must fail (<8)")
	}
	if err := validatePassword("12345678"); err != nil {
		t.Errorf("8-char password must pass: %v", err)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/httpapi/ -run 'TestValidateNewUser|TestValidatePasswordLen'`
Expected: FAIL (`validateNewUser` undefined).

- [ ] **Step 3: Add CRUD methods to `internal/store/users.go`**

```go
func (p *PGUsers) Update(ctx context.Context, id, email, display string, roles []string, disabled bool) error {
	tag, err := p.st.PG.Exec(ctx, `
		UPDATE users SET email=$2, display=$3, roles=$4, disabled=$5, updated_at=now() WHERE id=$1`,
		id, email, display, roles, disabled)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrUserNotFound
	}
	return err
}

func (p *PGUsers) SetPassword(ctx context.Context, id, hash string) error {
	tag, err := p.st.PG.Exec(ctx,
		`UPDATE users SET password_hash=$2, password_set_at=now() WHERE id=$1 AND auth_source='local'`, id, hash)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrUserNotFound
	}
	return err
}

func (p *PGUsers) Delete(ctx context.Context, id string) error {
	tag, err := p.st.PG.Exec(ctx, `DELETE FROM users WHERE id=$1`, id)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrUserNotFound
	}
	return err
}

// KeyCount returns how many active API keys the user owns (delete guard).
func (p *PGUsers) KeyCount(ctx context.Context, id string) (int, error) {
	var n int
	err := p.st.PG.QueryRow(ctx, `SELECT count(*) FROM api_keys WHERE user_id=$1 AND status='active'`, id).Scan(&n)
	return n, err
}

// ByID returns the row for self password-change (verify current password).
func (p *PGUsers) ByID(ctx context.Context, id string) (auth.UserRow, error) {
	var u auth.UserRow
	err := p.st.PG.QueryRow(ctx, `
		SELECT id::text, subject, email, display, roles, password_hash, disabled, auth_source
		FROM users WHERE id=$1`, id,
	).Scan(&u.ID, &u.Subject, &u.Email, &u.Display, &u.Roles, &u.PasswordHash, &u.Disabled, &u.AuthSource)
	return u, err
}

var ErrUserNotFound = errors.New("user not found")
```

- [ ] **Step 4: Create `internal/httpapi/api_users.go`** (handlers + pure validators)

```go
package httpapi

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/auth"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/store"
)

const minPasswordLen = 8

func validatePassword(pw string) error {
	if len(pw) < minPasswordLen {
		return fmt.Errorf("password must be at least %d characters", minPasswordLen)
	}
	return nil
}

func validateNewUser(username string, roles []string, password string, known map[string]bool) error {
	if username == "" {
		return errors.New("username is required")
	}
	if err := validatePassword(password); err != nil {
		return err
	}
	for _, r := range roles {
		if !known[r] {
			return fmt.Errorf("unknown role %q", r)
		}
	}
	return nil
}

// knownRoles loads role keys from roles_policy for validation.
func (s *Server) knownRoles(r *http.Request) (map[string]bool, error) {
	rows, err := s.st.PG.Query(r.Context(), `SELECT role FROM roles_policy`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var role string
		if err := rows.Scan(&role); err != nil {
			return nil, err
		}
		out[role] = true
	}
	return out, rows.Err()
}

func (s *Server) users() *store.PGUsers { return store.NewPGUsers(s.st) }

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	sess, _ := sessionFrom(r.Context())
	var body struct {
		Username string   `json:"username"`
		Email    string   `json:"email"`
		Display  string   `json:"display"`
		Roles    []string `json:"roles"`
		Password string   `json:"password"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeControlError(w, http.StatusBadRequest, "invalid body")
		return
	}
	known, err := s.knownRoles(r)
	if err != nil {
		writeControlError(w, http.StatusInternalServerError, "failed to load roles")
		return
	}
	if err := validateNewUser(body.Username, body.Roles, body.Password, known); err != nil {
		writeControlError(w, http.StatusBadRequest, err.Error())
		return
	}
	hash, err := auth.HashPassword(body.Password)
	if err != nil {
		writeControlError(w, http.StatusInternalServerError, "hash failed")
		return
	}
	id, err := s.users().CreateLocal(r.Context(), auth.UserRow{
		Subject: body.Username, Email: body.Email, Display: body.Display, Roles: body.Roles, PasswordHash: hash,
	})
	if err != nil {
		writeControlError(w, http.StatusBadRequest, "create failed (username taken?)")
		return
	}
	s.audit(r.Context(), sess.principal.Subject, "user.create", body.Username, map[string]any{"roles": body.Roles})
	writeJSON(w, http.StatusCreated, map[string]string{"id": id})
}

func (s *Server) handleUpdateUser(w http.ResponseWriter, r *http.Request) {
	sess, _ := sessionFrom(r.Context())
	id := r.PathValue("id")
	var body struct {
		Email    string   `json:"email"`
		Display  string   `json:"display"`
		Roles    []string `json:"roles"`
		Disabled bool     `json:"disabled"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeControlError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if err := s.guardLastAdmin(r, id, body.Roles, body.Disabled); err != nil {
		writeControlError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.users().Update(r.Context(), id, body.Email, body.Display, body.Roles, body.Disabled); err != nil {
		s.writeUserErr(w, err)
		return
	}
	s.audit(r.Context(), sess.principal.Subject, "user.update", id, map[string]any{"disabled": body.Disabled, "roles": body.Roles})
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleResetPassword(w http.ResponseWriter, r *http.Request) {
	sess, _ := sessionFrom(r.Context())
	id := r.PathValue("id")
	var body struct {
		Password string `json:"password"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeControlError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if err := validatePassword(body.Password); err != nil {
		writeControlError(w, http.StatusBadRequest, err.Error())
		return
	}
	hash, _ := auth.HashPassword(body.Password)
	if err := s.users().SetPassword(r.Context(), id, hash); err != nil {
		s.writeUserErr(w, err)
		return
	}
	s.audit(r.Context(), sess.principal.Subject, "user.password_reset", id, nil)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	sess, _ := sessionFrom(r.Context())
	id := r.PathValue("id")
	if err := s.guardLastAdmin(r, id, nil, true); err != nil {
		writeControlError(w, http.StatusBadRequest, err.Error())
		return
	}
	n, err := s.users().KeyCount(r.Context(), id)
	if err != nil {
		writeControlError(w, http.StatusInternalServerError, "failed to check keys")
		return
	}
	if n > 0 {
		writeControlError(w, http.StatusBadRequest, "user still owns active API keys; revoke them first")
		return
	}
	if err := s.users().Delete(r.Context(), id); err != nil {
		s.writeUserErr(w, err)
		return
	}
	s.audit(r.Context(), sess.principal.Subject, "user.delete", id, nil)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleChangeOwnPassword lets a local user change their own password.
func (s *Server) handleChangeOwnPassword(w http.ResponseWriter, r *http.Request) {
	sess, _ := sessionFrom(r.Context())
	var body struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeControlError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if err := validatePassword(body.New); err != nil {
		writeControlError(w, http.StatusBadRequest, err.Error())
		return
	}
	u, err := s.users().ByID(r.Context(), sess.userID)
	if err != nil {
		writeControlError(w, http.StatusInternalServerError, "failed to load user")
		return
	}
	if u.AuthSource != "local" || !auth.CheckPassword(u.PasswordHash, body.Current) {
		writeControlError(w, http.StatusBadRequest, "current password is incorrect")
		return
	}
	hash, _ := auth.HashPassword(body.New)
	if err := s.users().SetPassword(r.Context(), sess.userID, hash); err != nil {
		s.writeUserErr(w, err)
		return
	}
	s.audit(r.Context(), sess.principal.Subject, "user.self_password", sess.userID, nil)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// guardLastAdmin blocks an update/delete that would remove the final admin.
func (s *Server) guardLastAdmin(r *http.Request, id string, newRoles []string, disabling bool) error {
	u, err := s.users().ByID(r.Context(), id)
	if err != nil {
		return nil // not found -> let the underlying op report it
	}
	wasAdmin := false
	for _, role := range u.Roles {
		if role == auth.AdminRole {
			wasAdmin = true
		}
	}
	if !wasAdmin {
		return nil
	}
	stillAdmin := false
	for _, role := range newRoles {
		if role == auth.AdminRole {
			stillAdmin = true
		}
	}
	if disabling || !stillAdmin {
		n, _ := s.users().CountAdmins(r.Context())
		if n <= 1 {
			return errors.New("cannot remove or disable the last admin")
		}
	}
	return nil
}

func (s *Server) writeUserErr(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrUserNotFound) {
		writeControlError(w, http.StatusNotFound, "user not found")
		return
	}
	writeControlError(w, http.StatusInternalServerError, "operation failed")
}
```

- [ ] **Step 5: Register routes**

In `internal/httpapi/api_admin.go` near the existing `/api/admin/users`:

```go
s.mux.HandleFunc("POST /api/admin/users", a(s.handleCreateUser))
s.mux.HandleFunc("PUT /api/admin/users/{id}", a(s.handleUpdateUser))
s.mux.HandleFunc("POST /api/admin/users/{id}/password", a(s.handleResetPassword))
s.mux.HandleFunc("DELETE /api/admin/users/{id}", a(s.handleDeleteUser))
```

Extend `handleAdminUsers`'s SELECT + struct to include `disabled` and `auth_source` (add `disabled bool`, `auth_source text` to the query, struct, and `Scan`).

In `internal/httpapi/server.go` (control-plane, `requireSession`):

```go
s.mux.HandleFunc("POST /api/me/password", s.requireSession(s.handleChangeOwnPassword))
```

- [ ] **Step 6: Run tests + live-verify**

Run: `go test ./internal/httpapi/ -run 'TestValidate' && go build ./... && go vet ./...`
Then live (compose, as admin session): create a user, log in as it, change its password, fail to delete the last admin (expect 400), reset a user's password, delete a key-less user. Confirm each status.

- [ ] **Step 7: Commit**

```bash
git add internal/httpapi/api_users.go internal/store/users.go internal/httpapi/api_admin.go internal/httpapi/api_self.go internal/httpapi/server.go internal/httpapi/api_users_test.go
git commit -m "feat(auth): admin user CRUD + self password change"
```

---

### Task 6: OIDC provider

**Files:**
- Create: `internal/auth/oidc.go`
- Modify: `internal/config/config.go` (OIDC vars + validation), `cmd/ipsupport-airllm/main.go` (oidc wiring + routes), `go.mod`
- Test: `internal/auth/oidc_test.go`

**Interfaces:**
- Consumes: `Session`, `UserStore` (for lazy upsert of OIDC users — add `UpsertOIDC` to the interface + PG impl).
- Produces: `NewOIDCAuth(cfg OIDCConfig, store UserStore, sess *Session) (*OIDCAuth, error)`; HTTP handlers `LoginStart` and `Callback`; pure `parseRoles(claim any) []string` and `applyRoleMap(roles []string, m map[string]string) []string`.

- [ ] **Step 1: Add deps**

```bash
go get github.com/coreos/go-oidc/v3/oidc golang.org/x/oauth2
```

- [ ] **Step 2: Write the failing test** — `internal/auth/oidc_test.go`

```go
package auth

import (
	"reflect"
	"sort"
	"testing"
)

func TestParseRolesArrayAndObject(t *testing.T) {
	arr := parseRoles([]any{"airllm_admin", "airllm_user"})
	sort.Strings(arr)
	if !reflect.DeepEqual(arr, []string{"airllm_admin", "airllm_user"}) {
		t.Fatalf("array claim: %v", arr)
	}
	// Zitadel-style object whose KEYS are the roles.
	obj := parseRoles(map[string]any{"airllm_admin": map[string]any{"org": "x"}})
	if len(obj) != 1 || obj[0] != "airllm_admin" {
		t.Fatalf("object claim: %v", obj)
	}
	if parseRoles(nil) != nil {
		t.Fatal("nil claim must yield no roles")
	}
}

func TestApplyRoleMap(t *testing.T) {
	got := applyRoleMap([]string{"admins", "devs"}, map[string]string{"admins": "airllm_admin", "devs": "airllm_user"})
	sort.Strings(got)
	if !reflect.DeepEqual(got, []string{"airllm_admin", "airllm_user"}) {
		t.Fatalf("mapped roles: %v", got)
	}
	// no map -> identity
	id := applyRoleMap([]string{"airllm_admin"}, nil)
	if len(id) != 1 || id[0] != "airllm_admin" {
		t.Fatalf("identity map: %v", id)
	}
}
```

- [ ] **Step 3: Run to verify it fails**

Run: `go test ./internal/auth/ -run 'TestParseRoles|TestApplyRoleMap'`
Expected: FAIL (`parseRoles` undefined).

- [ ] **Step 4: Create `internal/auth/oidc.go`**

```go
package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/http"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// OIDCConfig is the generic OIDC relying-party configuration (all from env).
type OIDCConfig struct {
	Issuer       string
	ClientID     string
	ClientSecret string
	RedirectURL  string
	Scopes       []string
	RolesClaim   string
	RoleMap      map[string]string
}

// OIDCAuth authenticates via OpenID Connect and issues our HMAC session cookie.
type OIDCAuth struct {
	cfg      OIDCConfig
	store    UserStore
	verifier *oidc.IDTokenVerifier
	oauth    oauth2.Config
	*Session
}

// NewOIDCAuth performs discovery and builds the verifier/oauth config.
func NewOIDCAuth(ctx context.Context, cfg OIDCConfig, store UserStore, sess *Session) (*OIDCAuth, error) {
	provider, err := oidc.NewProvider(ctx, cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("oidc discovery: %w", err)
	}
	scopes := cfg.Scopes
	if len(scopes) == 0 {
		scopes = []string{oidc.ScopeOpenID, "profile", "email"}
	}
	return &OIDCAuth{
		cfg:      cfg,
		store:    store,
		verifier: provider.Verifier(&oidc.Config{ClientID: cfg.ClientID}),
		oauth: oauth2.Config{
			ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret,
			Endpoint: provider.Endpoint(), RedirectURL: cfg.RedirectURL, Scopes: scopes,
		},
		Session: sess,
	}, nil
}

// LoginStart begins the auth-code+PKCE flow.
func (o *OIDCAuth) LoginStart(w http.ResponseWriter, r *http.Request) {
	state := randURL()
	nonce := randURL()
	verifier := oauth2.GenerateVerifier()
	setTemp(w, "air_oidc_state", state)
	setTemp(w, "air_oidc_nonce", nonce)
	setTemp(w, "air_oidc_pkce", verifier)
	url := o.oauth.AuthCodeURL(state,
		oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier))
	http.Redirect(w, r, url, http.StatusFound)
}

// Callback completes the flow, upserts the user, and sets the session cookie.
func (o *OIDCAuth) Callback(w http.ResponseWriter, r *http.Request) {
	if c, _ := r.Cookie("air_oidc_state"); c == nil || c.Value != r.URL.Query().Get("state") {
		http.Error(w, "bad state", http.StatusBadRequest)
		return
	}
	pkce, _ := r.Cookie("air_oidc_pkce")
	tok, err := o.oauth.Exchange(r.Context(), r.URL.Query().Get("code"),
		oauth2.VerifierOption(pkceVal(pkce)))
	if err != nil {
		http.Error(w, "token exchange failed", http.StatusBadGateway)
		return
	}
	rawID, _ := tok.Extra("id_token").(string)
	idt, err := o.verifier.Verify(r.Context(), rawID)
	if err != nil {
		http.Error(w, "invalid id token", http.StatusUnauthorized)
		return
	}
	nonce, _ := r.Cookie("air_oidc_nonce")
	if nonce == nil || idt.Nonce != nonce.Value {
		http.Error(w, "bad nonce", http.StatusUnauthorized)
		return
	}
	var claims map[string]any
	if err := idt.Claims(&claims); err != nil {
		http.Error(w, "claims error", http.StatusUnauthorized)
		return
	}
	roles := applyRoleMap(parseRoles(claims[o.cfg.RolesClaim]), o.cfg.RoleMap)
	email, _ := claims["email"].(string)
	p := Principal{Subject: idt.Subject, Email: email, Roles: roles}
	if _, err := o.store.UpsertOIDC(r.Context(), p); err != nil {
		http.Error(w, "user upsert failed", http.StatusInternalServerError)
		return
	}
	o.SetSession(w, p)
	http.Redirect(w, r, "/", http.StatusFound)
}

// parseRoles reads roles from a claim that is either a string array or an
// object whose keys are role names (Zitadel project-roles claim).
func parseRoles(claim any) []string {
	switch v := claim.(type) {
	case []any:
		out := make([]string, 0, len(v))
		for _, x := range v {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case map[string]any:
		out := make([]string, 0, len(v))
		for k := range v {
			out = append(out, k)
		}
		return out
	default:
		return nil
	}
}

// applyRoleMap maps IdP role names to airllm roles; a nil map is identity.
func applyRoleMap(roles []string, m map[string]string) []string {
	if len(m) == 0 {
		return roles
	}
	out := make([]string, 0, len(roles))
	for _, r := range roles {
		if mapped, ok := m[r]; ok {
			out = append(out, mapped)
		}
	}
	return out
}

func randURL() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func setTemp(w http.ResponseWriter, name, val string) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: val, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, MaxAge: int((10 * time.Minute).Seconds())})
}

func pkceVal(c *http.Cookie) string {
	if c == nil {
		return ""
	}
	return c.Value
}
```

Add `UpsertOIDC` to `auth.UserStore` (Task 2 interface) and to `store.PGUsers`:

```go
// in internal/auth/store.go UserStore:
UpsertOIDC(ctx context.Context, p Principal) (string, error)

// in internal/store/users.go:
func (p *PGUsers) UpsertOIDC(ctx context.Context, pr auth.Principal) (string, error) {
	var id string
	err := p.st.PG.QueryRow(ctx, `
		INSERT INTO users (subject, email, display, roles, auth_source)
		VALUES ($1, $2, $1, $3, 'oidc')
		ON CONFLICT (subject) DO UPDATE SET email=EXCLUDED.email, roles=EXCLUDED.roles, updated_at=now()
		RETURNING id::text`, pr.Subject, pr.Email, pr.Roles).Scan(&id)
	return id, err
}
```

> The `fakeUsers` in `local_test.go` must gain a no-op `UpsertOIDC` to keep satisfying `UserStore`.

- [ ] **Step 5: OIDC config** — `internal/config/config.go`

Add an `OIDC OIDCConfig` field (mirror of `auth.OIDCConfig` to avoid a config→auth import cycle — config defines its own struct and main maps it). In `Load`, when `c.AuthMode == "oidc"`, read and require: `OIDC_ISSUER`, `OIDC_CLIENT_ID`, `OIDC_CLIENT_SECRET`, `OIDC_REDIRECT_URL`; parse `OIDC_SCOPES` (space-separated, default `openid profile email`), `OIDC_ROLES_CLAIM` (required), `OIDC_ROLE_MAP` (`a:b,c:d`). Return an error naming any missing required var.

```go
func parseRoleMap(s string) map[string]string {
	out := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		if k, v, ok := strings.Cut(strings.TrimSpace(pair), ":"); ok && k != "" {
			out[k] = v
		}
	}
	return out
}
```

Add a config test: `AUTH_MODE=oidc` with no OIDC vars → error; with all required set → ok.

- [ ] **Step 6: Wire oidc in `main.go`**

```go
case "oidc":
	oa, err := auth.NewOIDCAuth(ctx, auth.OIDCConfig{
		Issuer: cfg.OIDC.Issuer, ClientID: cfg.OIDC.ClientID, ClientSecret: cfg.OIDC.ClientSecret,
		RedirectURL: cfg.OIDC.RedirectURL, Scopes: cfg.OIDC.Scopes,
		RolesClaim: cfg.OIDC.RolesClaim, RoleMap: cfg.OIDC.RoleMap,
	}, store.NewPGUsers(st), session)
	if err != nil {
		return fmt.Errorf("oidc init: %w", err)
	}
	deps.Auth = oa
	deps.OIDC = oa // new optional field on Deps so the server can register routes
```

Add an `OIDC` field to `httpapi.Deps` typed
`interface{ LoginStart(http.ResponseWriter, *http.Request); Callback(http.ResponseWriter, *http.Request) }`,
add a matching `oidc` field to the `Server` struct, and assign it in `NewServer`
(`s.oidc = deps.OIDC`). Then register when non-nil:

```go
if s.oidc != nil {
	s.mux.HandleFunc("GET /auth/sso", s.oidc.LoginStart)
	s.mux.HandleFunc("GET /auth/callback", s.oidc.Callback)
}
```

- [ ] **Step 7: Run tests + build**

Run: `go test ./internal/auth/ ./internal/config/ && go build ./... && go vet ./... && go test -race ./...`
Expected: PASS. (Full e2e against a live Zitadel is deferred to the k8s-deploy sub-project; unit tests cover roles parsing/mapping; ID-token verification is exercised by go-oidc's own tests + the mock-IdP test below if added.)

- [ ] **Step 8: Commit**

```bash
git add internal/auth/oidc.go internal/auth/store.go internal/store/users.go internal/config/config.go cmd/ipsupport-airllm/main.go internal/httpapi/server.go internal/auth/oidc_test.go internal/auth/local_test.go go.mod go.sum
git commit -m "feat(auth): generic OIDC provider (go-oidc + oauth2, PKCE)"
```

---

### Task 7: Console — users CRUD, change-password, SSO button

**Files:**
- Modify: `web/static/app.js`
- Test: live (Playwright click-through + screenshot)

**Interfaces:**
- Consumes: `GET/POST/PUT/DELETE /api/admin/users[...]`, `POST /api/admin/users/{id}/password`, `POST /api/me/password`, `GET /api/auth/mode`.

- [ ] **Step 1: Admin users tab — add create/edit/reset/delete**

In `adminUsers(c)` (the users tab render): add a "New user" button and per-row Edit / Reset password / Delete buttons. New/Edit open a `modalForm` with username (new only), email, display, a roles multi-select (from `GET /api/admin/roles`), disabled checkbox, and password (new + reset). On submit call the matching endpoint, then re-render. Show the server's error text on failure (e.g. last-admin guard, username taken).

- [ ] **Step 2: Change-password affordance**

In the sidebar footer (near "Sign out"), add a "Change password" link that opens a `modalForm` with `current` + `new` fields → `POST /api/me/password`. Hide it when `GET /api/auth/mode` reports `oidc` (no local password).

- [ ] **Step 3: SSO login button**

On the login screen, fetch `GET /api/auth/mode`; if `mode === "oidc"`, hide the password form and render a "Sign in with SSO" button linking to `sso_url`. If `local`, render the password form as today.

- [ ] **Step 4: Rebuild + live-verify**

Run: rebuild the app image, then Playwright (`e2e/`) console click-through stays green, and screenshot the admin users tab to confirm CRUD + the change-password modal render. Manually: create a user in the UI, log in as it, change its password.

- [ ] **Step 5: Commit**

```bash
git add web/static/app.js
git commit -m "feat(console): user management, change password, SSO login button"
```

---

### Task 8: Docs

**Files:**
- Modify: `docs/configuration.md`, `docs/api.md`, `docs/getting-started.md`, `docs/operations.md`

- [ ] **Step 1: configuration.md** — document `AUTH_MODE` (`local | oidc`, `mock` deprecated), `AIRLLM_SESSION_KEY` (optional; derived from master key), `AIRLLM_ADMIN_USERNAME` / `AIRLLM_ADMIN_PASSWORD`, and all `OIDC_*` vars with placeholder values.

- [ ] **Step 2: api.md** — add `GET /api/auth/mode`, `GET /auth/sso`, `GET /auth/callback`, the admin user CRUD routes, and `POST /api/me/password`, each with its auth tier.

- [ ] **Step 3: getting-started.md** — replace "random per-boot password" with: bootstrap admin from `AIRLLM_ADMIN_PASSWORD` (or random logged once), persistent across restarts; how to add users; how to enable OIDC.

- [ ] **Step 4: operations.md** — security posture: session key stability, admin bootstrap, OIDC behind ingress, that disabling a user doesn't auto-revoke keys.

- [ ] **Step 5: Verify links + commit**

Run: `grep -rn "AUTH_MODE\|AIRLLM_SESSION_KEY\|/api/auth/mode" docs/` to confirm coverage.

```bash
git add docs/
git commit -m "docs: auth modes, session key, admin bootstrap, user management, OIDC"
```

---

## Notes for the executor

- No DB unit-test harness exists; PG impls (`PGUsers`) and HTTP endpoints are verified **live** against the compose stack, exactly as the capture store is. Keep that split: pure logic (codec, bcrypt, LocalAuth with a fake store, bootstrap, validators, roles parsing/mapping) is unit-tested; persistence/wiring is live-verified with documented curl/Playwright steps.
- After Task 4, the live "restart keeps the same admin password" check is the headline acceptance test for the whole feature.
- Run `go test -race ./...`, `go vet ./...`, `gofmt -l` before each commit.
