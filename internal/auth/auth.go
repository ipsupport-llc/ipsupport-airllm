// Package auth abstracts control-plane authentication. The production
// implementation is generic OIDC (wired on the k8s deploy); local password
// auth is provided by LocalAuth using bcrypt hashes and HMAC session cookies.
package auth

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"time"
)

// Roles.
const (
	AdminRole   = "airllm_admin"
	UserRole    = "airllm_user"
	AuditorRole = "airllm_auditor"
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

// IsAuditor reports whether the principal holds the auditor or admin role.
func (p Principal) IsAuditor() bool { return p.HasRole(AuditorRole) || p.HasRole(AdminRole) }

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

// payload is the signed session token body.
type payload struct {
	Sub   string   `json:"sub"`
	Email string   `json:"email"`
	Roles []string `json:"roles"`
	Exp   int64    `json:"exp"`
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

func randToken(n int) string {
	return hex.EncodeToString(randBytes(n))
}
