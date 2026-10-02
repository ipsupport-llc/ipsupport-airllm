package auth

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSessionRoundTrip(t *testing.T) {
	s := NewSession([]byte("0123456789abcdef0123456789abcdef"))
	p := Principal{Subject: "admin", Email: "a@b", Roles: []string{AdminRole}}

	rec := httptest.NewRecorder()
	s.SetSession(rec, httptest.NewRequest("GET", "/", nil), p)
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
	NewSession(key).SetSession(rec, httptest.NewRequest("GET", "/", nil), Principal{Subject: "x"})
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(rec.Result().Cookies()[0])
	if _, err := NewSession(key).Authenticate(req); err != nil {
		t.Fatalf("a cookie signed by one instance must verify on another with the same key: %v", err)
	}
}

// TestSetSessionSecureFlag proves the cookie's Secure attribute tracks the
// ORIGINAL client request's scheme (Auth I1 fix): plain HTTP (local dev,
// nothing set) gets no Secure flag — setting it unconditionally would
// silently break every HTTP-only dev/test setup, since browsers refuse to
// send a Secure cookie back over plain HTTP. A direct TLS connection, or a
// terminating reverse proxy reporting X-Forwarded-Proto: https (what both
// of this project's real deployment topologies, Caddy and Traefik, do by
// default), gets Secure.
func TestSetSessionSecureFlag(t *testing.T) {
	s := NewSession([]byte("0123456789abcdef0123456789abcdef"))
	p := Principal{Subject: "x"}

	cases := []struct {
		name   string
		req    *http.Request
		secure bool
	}{
		{"plain http, no proxy", httptest.NewRequest("GET", "/", nil), false},
		{"direct TLS connection", func() *http.Request {
			r := httptest.NewRequest("GET", "/", nil)
			r.TLS = &tls.ConnectionState{}
			return r
		}(), true},
		{"behind a TLS-terminating proxy", func() *http.Request {
			r := httptest.NewRequest("GET", "/", nil)
			r.Header.Set("X-Forwarded-Proto", "https")
			return r
		}(), true},
		{"proxy reports http", func() *http.Request {
			r := httptest.NewRequest("GET", "/", nil)
			r.Header.Set("X-Forwarded-Proto", "http")
			return r
		}(), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			s.SetSession(rec, c.req, p)
			cookie := rec.Result().Cookies()[0]
			if cookie.Secure != c.secure {
				t.Errorf("Secure = %v, want %v", cookie.Secure, c.secure)
			}
		})
	}
}

func TestSessionRejectsTamperAndWrongKey(t *testing.T) {
	rec := httptest.NewRecorder()
	NewSession([]byte("0123456789abcdef0123456789abcdef")).SetSession(rec, httptest.NewRequest("GET", "/", nil), Principal{Subject: "x"})
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
