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
