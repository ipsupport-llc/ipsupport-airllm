package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/store"
)

// tokenEndpoint is a stand-in for Google's OAuth2 token endpoint: it hands
// out a token and counts how many exchanges it was asked for. Everything
// here runs offline — a service-account credential naming this endpoint as
// its token_uri never touches the real one.
func tokenEndpoint(t *testing.T, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"ya29.stub","token_type":"Bearer","expires_in":3600}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// serviceAccountJSON builds a syntactically complete service-account
// credential. id makes it unique, which matters because token sources are
// cached process-wide by credential: two tests sharing a credential would
// share a cache entry and each other's exchange count.
func serviceAccountJSON(t *testing.T, id, tokenURI string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]string{
		"type":                        "service_account",
		"project_id":                  "acme",
		"private_key_id":              id,
		"private_key":                 testPrivateKeyPEM,
		"client_email":                id + "@acme.iam.gserviceaccount.com",
		"token_uri":                   tokenURI,
		"client_id":                   "1",
		"auth_provider_x509_cert_url": "https://www.googleapis.com/oauth2/v1/certs",
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestGoogleTokenSourceSurvivesTheRequestThatBuiltIt is the lifecycle trap
// worth a test of its own. The registry is rebuilt from an admin request
// whose context is cancelled the moment that request completes; a token
// source that captured it would fail every refresh from then on, and an
// operator would see a provider that worked at save time start failing
// minutes later for no visible reason.
func TestGoogleTokenSourceSurvivesTheRequestThatBuiltIt(t *testing.T) {
	var hits atomic.Int32
	srv := tokenEndpoint(t, &hits)
	cred := serviceAccountJSON(t, "detached", srv.URL)

	// Stand in for the admin request: cancelled as soon as it is served.
	adminCtx, cancel := context.WithCancel(context.Background())
	ts, err := GoogleTokenSource(adminCtx, cred)
	if err != nil {
		t.Fatalf("GoogleTokenSource: %v", err)
	}
	cancel()

	// A context of its own, so a failure here can only come from the source.
	tok, err := ts.Token(context.Background())
	if err != nil {
		t.Fatalf("the token source did not outlive the request that built it: %v", err)
	}
	if tok != "ya29.stub" {
		t.Errorf("token = %q, want the one the endpoint issued", tok)
	}
}

// TestGoogleTokenSourceFallsBackToTheAmbientIdentity covers the default
// configuration: nothing stored, so the gateway authenticates as whatever
// identity its environment provides — the pod's own federated identity in the
// cluster, and here a credential file named the way the cluster names one.
// The point is that an absent credential is a valid configuration, not a
// broken one.
func TestGoogleTokenSourceFallsBackToTheAmbientIdentity(t *testing.T) {
	var hits atomic.Int32
	srv := tokenEndpoint(t, &hits)

	path := filepath.Join(t.TempDir(), "ambient.json")
	if err := os.WriteFile(path, serviceAccountJSON(t, "ambient", srv.URL), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", path)

	ts, err := GoogleTokenSource(context.Background(), nil)
	if err != nil {
		t.Fatalf("an empty credential must resolve the ambient identity, not fail: %v", err)
	}
	if _, err := ts.Token(context.Background()); err != nil {
		t.Fatalf("token: %v", err)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("token endpoint called %d times, want 1 — the exchange did not go to the ambient credential", got)
	}
}

// TestGoogleTokenSourceReusesTheExchange covers the second lifecycle detail:
// an ordinary provider edit rebuilds the registry, and rebuilding must not
// hammer the token endpoint with a fresh exchange each time.
func TestGoogleTokenSourceReusesTheExchange(t *testing.T) {
	var hits atomic.Int32
	srv := tokenEndpoint(t, &hits)
	cred := serviceAccountJSON(t, "reused", srv.URL)

	for i := range 3 {
		ts, err := GoogleTokenSource(context.Background(), cred)
		if err != nil {
			t.Fatalf("rebuild %d: %v", i, err)
		}
		if _, err := ts.Token(context.Background()); err != nil {
			t.Fatalf("rebuild %d: token: %v", i, err)
		}
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("token endpoint was called %d times across 3 registry rebuilds, want 1", got)
	}
}

func TestGoogleTokenSourceDistinguishesCredentials(t *testing.T) {
	var hits atomic.Int32
	srv := tokenEndpoint(t, &hits)

	a, err := GoogleTokenSource(context.Background(), serviceAccountJSON(t, "distinct-a", srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	b, err := GoogleTokenSource(context.Background(), serviceAccountJSON(t, "distinct-b", srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Error("two different credentials must not share a token source; they authenticate as different accounts")
	}
}

// TestGoogleTokenSourceRejectsUnusableCredentials is the security-relevant
// half: unusable bytes must be an error, never a quiet fall back to the
// ambient identity. Falling back would run the gateway as a different
// principal than the operator configured.
func TestGoogleTokenSourceRejectsUnusableCredentials(t *testing.T) {
	for _, cred := range []string{
		"not json at all",
		`{"type":"nonsense"}`,
	} {
		if _, err := GoogleTokenSource(context.Background(), []byte(cred)); err == nil {
			t.Errorf("credential %q resolved instead of failing; a fall back to the ambient identity is worse than no provider", cred)
		}
	}
}

// TestNewVertexFromRowDisablesRatherThanGuesses covers the decisions the
// registry loader delegates: every one of them ends with the provider absent
// and an error logged, so the gateway still starts and every other provider
// keeps working.
//
// Each case fails before a token source is ever resolved, which is what keeps
// this test independent of whether the machine running it has cloud
// credentials at all.
func TestNewVertexFromRowDisablesRatherThanGuesses(t *testing.T) {
	cases := []struct {
		name    string
		row     store.ProviderRow
		credErr error
	}{
		{
			"a credential that cannot be decrypted",
			store.ProviderRow{Name: "vx", Kind: "vertex", Config: json.RawMessage(`{"project":"acme"}`)},
			errors.New("cipher: message authentication failed"),
		},
		{
			"a malformed configuration",
			store.ProviderRow{Name: "vx", Kind: "vertex", Config: json.RawMessage(`{"project":`)},
			nil,
		},
		{
			"neither a project nor an address",
			store.ProviderRow{Name: "vx", Kind: "vertex", Config: json.RawMessage(`{}`)},
			nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := newVertexFromRow(context.Background(), c.row, nil, c.credErr)
			if err == nil {
				t.Fatalf("want the provider disabled with an error, got %+v", got)
			}
		})
	}
}

// testPrivateKeyPEM is a throwaway 2048-bit RSA key, here only so a
// service-account credential is structurally complete. It signs nothing that
// leaves the process: the stub token endpoint does not verify the assertion.
const testPrivateKeyPEM = `-----BEGIN PRIVATE KEY-----
MIIEvgIBADANBgkqhkiG9w0BAQEFAASCBKgwggSkAgEAAoIBAQCQRUIJTTrGoSR1
nOOO8BHgM1UqIB4ABh0ltZuZlVIBPfMdy0ErBJA4fcsk6FhCpqIzby8yT3iHY4PG
zgIuId/VixHG4gEKbps7MOcG3pePtXZGO9uxsbLkjWju+q+f+j2iypdkG7Ohlvus
Gti7jY6pVDuOVbB3oletl5zNAuZacSMxedck/vl2FFtG7nboPd3bx0egJaT1v0jm
LBTIuvH2qeOv9GkjbxddfEgifaAxqBE0Eytf4ukFXdVkGYk+NYg+4nK+9rDpvVNO
+ou/rW+78GMQCUQoFhLChW29rLngdWY+IWN0U/RF6A1kG8NqKKJGZPiMQb30tdON
oc2KzMHTAgMBAAECggEANmk4f6OV8EXkJ0t1c3pNc55Il2unhODJa2hz99eeJwPD
RlBbEqtU7UlcLV5Hs1N/RyC+zx2z2nQIxhj6L4XtEm+x0613MQUIHKnT5/5ZcQTC
R7jZocngK1y936vCQvaw+k2oDUR5Wg9EeeNiLFI2JNy03Xip5mTe5oSQya03TZJe
w+SMtkiFAGwGFnrfGOQEvcB9Rk/aRFrVuF7/W2xYDqAzSlCe/0eTKKUQfjuq0N6p
pFwltOrPibq7apM+LduIVXNKM+HAPZoJr5w42xFhaNJ3JGyCkZvVpCIcXUXoH+ZV
9je+IKui6m8EYWDgM/w2/os2yqDynn6xB36tKHJxOQKBgQDK3i2DKMPZWVgIihuJ
5YrR1citaDnO2SXZAbvr36LjVrnB2b/JNo9jDG4gEenhbJ3a3zaSF1wCPqTKWeDe
OaSEkTSLq6K439flzM6t4JOmexd82/V5mi7uOYFidHABs0aGpy4idjEcvsTIe0K4
AHKFD8aj3NG/FWzGdrN1nMmRpQKBgQC2DkOMmYYmfi/Kc6aCCxu4mSM0Y1kMKf1/
ZR6UH9XJWHB1lIAIVnMdTkRSzldEzBaVdmk352GJxeGPJLVXOgiWNoCdL2WIlKjg
V8e1EkCxgIZfgjSiVf2FqnDyAqukMwpgOzLNZs8RAoY5Zgh+1eNyrw+K2E9Thxm+
gdoWmvY8FwKBgQCj6uXnZpbpFhHVxJH/2CNU7WKbCu46vqagM5B+RFM/UiICCkm2
8YjmRXLuIstRxAvAgD99x7YmciuA/SJ/LSBLpXBJssNmkifGnLgbMqzbBfaygqBU
Q0rMXla3ENI37X1867SRT+LbESG7xCzitCnUbizY1mH7/fnIWr0iuS79qQKBgQC0
DNFTiTY6dYvwToZ7kF7fJ1zA4AxeUlzqFGi0l/OISNYYA0DIfi8k6ZX6yyVV3f3r
3YrcBhLZ/gFA304VMUjyvn5edlSVSmjmTwoskxu2MOU0KgLCFgdAnbtMLcXxA6Wc
XI+2wpnBOdzjgXyfbAuhDW9yotF5S2Dzn1rABovGCwKBgCwO0oL1HAdDYCaHw6HU
RahytFsA1QNQOPSYgbJNqWWTISTqStlkiGtPebTTV5q3Tol5SzDj2CnhVGLQFUzm
I7i5X3qE3V+t5IRhq+tDJ5vtY6jk7+YbhJmzkf5H1EzC/q5T3RItL7K/y8uBsfft
y56d+wTVlPvMXgHMRaAe/Mgf
-----END PRIVATE KEY-----
`
