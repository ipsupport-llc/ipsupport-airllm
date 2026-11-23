package auth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

// recordingUsers is a UserStore that records UpsertOIDC calls.
type recordingUsers struct {
	fakeUsers
	upserted []Principal
}

func (r *recordingUsers) UpsertOIDC(_ context.Context, p Principal) (string, error) {
	r.upserted = append(r.upserted, p)
	return "oidc-user-id", nil
}

// fakeIdP is a minimal OIDC provider: discovery, JWKS, and a token endpoint
// that returns an RS256 id_token for the configured subject and nonce.
type fakeIdP struct {
	srv   *httptest.Server
	key   *rsa.PrivateKey
	nonce string
}

func newFakeIdP(t *testing.T, clientID, subject string, roles []string) *fakeIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIdP{key: key}
	mux := http.NewServeMux()
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	b64 := base64.RawURLEncoding.EncodeToString
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                f.srv.URL,
			"authorization_endpoint":                f.srv.URL + "/auth",
			"token_endpoint":                        f.srv.URL + "/token",
			"jwks_uri":                              f.srv.URL + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "alg": "RS256", "use": "sig", "kid": "k1",
			"n": b64(key.N.Bytes()), "e": b64(big.NewInt(int64(key.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		hdr, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "k1", "typ": "JWT"})
		claims, _ := json.Marshal(map[string]any{
			"iss": f.srv.URL, "sub": subject, "aud": clientID, "nonce": f.nonce,
			"email": subject + "@example.com", "roles": roles,
			"iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(),
		})
		signing := b64(hdr) + "." + b64(claims)
		sum := sha256.Sum256([]byte(signing))
		sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "at", "token_type": "Bearer", "expires_in": 3600,
			"id_token": signing + "." + b64(sig),
		})
	})
	return f
}

// TestOIDCCallbackProvisionsNewUser proves a first-time OIDC login creates
// the user (UpsertOIDC) and that the issued session carries the new user's
// id — the session path no longer creates users, so the callback is the
// only place an OIDC user is provisioned.
func TestOIDCCallbackProvisionsNewUser(t *testing.T) {
	const clientID = "airllm-test"
	idp := newFakeIdP(t, clientID, "new-sso-user", []string{AdminRole})
	users := &recordingUsers{}
	sess := NewSession([]byte("0123456789abcdef0123456789abcdef"))
	oa, err := NewOIDCAuth(context.Background(), OIDCConfig{
		Issuer: idp.srv.URL, ClientID: clientID, ClientSecret: "s",
		RedirectURL: "http://app.test/auth/callback", RolesClaim: "roles",
	}, users, sess)
	if err != nil {
		t.Fatalf("NewOIDCAuth: %v", err)
	}

	// LoginStart sets the state/nonce/PKCE cookies the callback checks.
	start := httptest.NewRecorder()
	oa.LoginStart(start, httptest.NewRequest(http.MethodGet, "/auth/login", nil))
	var state string
	cb := httptest.NewRequest(http.MethodGet, "/", nil)
	for _, c := range start.Result().Cookies() {
		cb.AddCookie(c)
		switch c.Name {
		case "air_oidc_state":
			state = c.Value
		case "air_oidc_nonce":
			idp.nonce = c.Value
		}
	}
	cb.URL, _ = url.Parse("/auth/callback?code=abc&state=" + url.QueryEscape(state))

	rec := httptest.NewRecorder()
	oa.Callback(rec, cb)
	if rec.Code != http.StatusFound {
		t.Fatalf("callback status = %d, body %q", rec.Code, rec.Body.String())
	}
	if len(users.upserted) != 1 || users.upserted[0].Subject != "new-sso-user" || !users.upserted[0].IsAdmin() {
		t.Fatalf("expected one UpsertOIDC for new-sso-user as admin, got %+v", users.upserted)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	for _, c := range rec.Result().Cookies() {
		if c.Name == cookieName {
			req.AddCookie(c)
		}
	}
	p, err := sess.Authenticate(req)
	if err != nil {
		t.Fatalf("session cookie not accepted: %v", err)
	}
	if p.Subject != "new-sso-user" || p.UserID != "oidc-user-id" {
		t.Fatalf("session principal = %+v, want subject new-sso-user and user id oidc-user-id", p)
	}
}
