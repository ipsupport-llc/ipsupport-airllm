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
