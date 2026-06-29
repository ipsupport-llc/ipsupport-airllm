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
