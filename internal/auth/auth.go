// Package auth abstracts control-plane authentication. The production
// implementation is generic OIDC (wired on the k8s deploy); the local mock
// is a username/password login that issues a signed session cookie, so the
// admin/self-service UI can be exercised realistically without an IdP.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

// Roles.
const (
	AdminRole = "airouter_admin"
	UserRole  = "airouter_user"
)

const (
	cookieName = "air_session"
	sessionTTL = 12 * time.Hour
)

// ErrNoSession indicates a missing or invalid session.
var ErrNoSession = errors.New("no valid session")

// Principal is an authenticated control-plane user.
type Principal struct {
	Subject string
	Email   string
	Roles   []string
}

// HasRole reports whether the principal holds role.
func (p Principal) HasRole(role string) bool {
	for _, r := range p.Roles {
		if r == role {
			return true
		}
	}
	return false
}

// IsAdmin reports whether the principal holds the admin role.
func (p Principal) IsAdmin() bool { return p.HasRole(AdminRole) }

// Authenticator resolves the principal for a request (from a session cookie).
type Authenticator interface {
	Authenticate(r *http.Request) (Principal, error)
}

// LoginProvider handles password login and session cookie lifecycle.
type LoginProvider interface {
	Login(username, password string) (Principal, bool)
	SetSession(w http.ResponseWriter, p Principal)
	ClearSession(w http.ResponseWriter)
}

// Credential is a mock login the operator can use; printed at startup.
type Credential struct {
	Username string
	Password string
	Admin    bool
}

type mockUser struct {
	password  string
	principal Principal
}

// Mock is a development authenticator: username/password login (random
// passwords generated at boot) plus HMAC-signed session cookies. NOT for
// production — generic OIDC replaces it on the k8s deploy.
type Mock struct {
	users      map[string]mockUser
	signingKey []byte
}

// NewMock builds the mock with an admin and a non-admin (operator) user,
// each with a freshly generated random password, and a random signing key.
// The returned credentials should be logged so the operator can sign in.
func NewMock() (*Mock, []Credential) {
	adminPw := randToken(18)
	opPw := randToken(18)
	m := &Mock{
		users: map[string]mockUser{
			"admin": {
				password:  adminPw,
				principal: Principal{Subject: "admin", Email: "admin@local", Roles: []string{AdminRole}},
			},
			"operator": {
				password:  opPw,
				principal: Principal{Subject: "operator", Email: "operator@local", Roles: []string{UserRole}},
			},
		},
		signingKey: randBytes(32),
	}
	return m, []Credential{
		{Username: "admin", Password: adminPw, Admin: true},
		{Username: "operator", Password: opPw, Admin: false},
	}
}

// Login validates credentials and returns the principal on success.
func (m *Mock) Login(username, password string) (Principal, bool) {
	u, ok := m.users[username]
	if !ok {
		return Principal{}, false
	}
	if subtle.ConstantTimeCompare([]byte(u.password), []byte(password)) != 1 {
		return Principal{}, false
	}
	return u.principal, true
}

// SetSession writes a signed session cookie for the principal.
func (m *Mock) SetSession(w http.ResponseWriter, p Principal) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    m.sign(payload{Sub: p.Subject, Email: p.Email, Roles: p.Roles, Exp: time.Now().Add(sessionTTL).Unix()}),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionTTL.Seconds()),
	})
}

// ClearSession expires the session cookie.
func (m *Mock) ClearSession(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", HttpOnly: true, MaxAge: -1})
}

// Authenticate validates the session cookie and returns its principal.
func (m *Mock) Authenticate(r *http.Request) (Principal, error) {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return Principal{}, ErrNoSession
	}
	body, sig, ok := strings.Cut(c.Value, ".")
	if !ok {
		return Principal{}, ErrNoSession
	}
	if !hmac.Equal([]byte(sig), []byte(m.macOf(body))) {
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

type payload struct {
	Sub   string   `json:"sub"`
	Email string   `json:"email"`
	Roles []string `json:"roles"`
	Exp   int64    `json:"exp"`
}

func (m *Mock) sign(p payload) string {
	b, _ := json.Marshal(p)
	body := base64.RawURLEncoding.EncodeToString(b)
	return body + "." + m.macOf(body)
}

func (m *Mock) macOf(body string) string {
	mac := hmac.New(sha256.New, m.signingKey)
	mac.Write([]byte(body))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

func randToken(n int) string {
	return hex.EncodeToString(randBytes(n))
}
