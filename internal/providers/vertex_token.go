package providers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sync"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// TokenSource mints the bearer token for an upstream whose credential is
// short-lived rather than a static key. It is consulted per request, so a
// token that expires mid-life is refreshed without rebuilding the registry.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// cloudPlatformScope is the OAuth2 scope the Vertex model API accepts.
const cloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"

// googleTokenSources caches resolved sources by a fingerprint of scope and
// credential, so rebuilding the registry — which an ordinary provider edit
// does — reuses the live access token instead of forcing a fresh exchange
// against the token endpoint on every save. Bounded in practice by the number
// of distinct credentials configured, which is the number of providers.
var (
	googleTokenMu      sync.Mutex
	googleTokenSources = map[string]TokenSource{}
)

// GoogleTokenSource resolves a Google OAuth2 token source with the
// cloud-platform scope: from credJSON when it is non-empty, and otherwise
// from the ambient default credentials — the pod's own federated identity in
// the cluster, a developer's own credentials on a laptop.
//
// Credential bytes that do not resolve are an error and never a fall back to
// the ambient identity. Falling back would run the gateway as a different
// principal than the operator configured, which is the one failure mode worse
// than the provider not working at all.
//
// Two lifecycle details no caller can see from the outside:
//
//   - The returned source refreshes for as long as the registry lives, so it
//     must not inherit the caller's context. The registry is rebuilt from an
//     admin request whose context is cancelled the moment that request
//     completes, and a source built with it would fail every refresh
//     afterwards — minutes later, for no visible reason. The context is
//     detached here rather than at each call site, because forgetting it at
//     one call site reproduces the bug.
//   - Only successful resolutions are cached, so a source that could not be
//     resolved is retried on the next rebuild rather than failing forever.
func GoogleTokenSource(ctx context.Context, credJSON []byte) (TokenSource, error) {
	fp := googleTokenFingerprint(credJSON)

	googleTokenMu.Lock()
	defer googleTokenMu.Unlock()
	if ts, ok := googleTokenSources[fp]; ok {
		return ts, nil
	}

	ctx = context.WithoutCancel(ctx)
	var (
		creds *google.Credentials
		err   error
	)
	if len(credJSON) > 0 {
		creds, err = google.CredentialsFromJSON(ctx, credJSON, cloudPlatformScope)
	} else {
		creds, err = google.FindDefaultCredentials(ctx, cloudPlatformScope)
	}
	if err != nil {
		return nil, err
	}

	ts := oauth2TokenSource{ts: creds.TokenSource}
	googleTokenSources[fp] = ts
	return ts, nil
}

// googleTokenFingerprint identifies a token source by what it authenticates
// as. The credential is hashed rather than kept, so the cache key is not
// itself a copy of the secret.
func googleTokenFingerprint(credJSON []byte) string {
	h := sha256.New()
	h.Write([]byte(cloudPlatformScope))
	h.Write([]byte{0})
	h.Write(credJSON)
	return hex.EncodeToString(h.Sum(nil))
}

// oauth2TokenSource adapts an oauth2.TokenSource, which caches and refreshes
// the token itself. x/oauth2 has no context-taking Token method — the source
// carries the detached context it was built with — so ctx is unused here by
// design rather than by oversight.
type oauth2TokenSource struct{ ts oauth2.TokenSource }

func (o oauth2TokenSource) Token(context.Context) (string, error) {
	tok, err := o.ts.Token()
	if err != nil {
		return "", err
	}
	return tok.AccessToken, nil
}
