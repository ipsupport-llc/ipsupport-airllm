package providers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sync"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"golang.org/x/sync/singleflight"
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
// against the token endpoint on every save. PruneGoogleTokenSources keeps it
// bounded by the number of currently-configured credentials rather than
// every one ever configured (a rotated-away credential would otherwise
// accumulate forever).
//
// googleTokenGroup deduplicates concurrent resolutions of the SAME
// fingerprint without serializing resolutions of a DIFFERENT one:
// googleTokenMu itself is only ever held for quick map access, never across
// the slow ambient-credential resolution below, so an unrelated provider's
// token lookup is never stalled by this one's.
var (
	googleTokenMu      sync.Mutex
	googleTokenSources = map[string]TokenSource{}
	googleTokenGroup   singleflight.Group
)

// resolveGoogleCredentials is the actual ambient/explicit credential
// resolution call, indirected through a package variable so a test can
// substitute a slow, controllable stand-in without making a real network
// call or depending on this machine's ambient identity.
var resolveGoogleCredentials = func(ctx context.Context, credJSON []byte) (*google.Credentials, error) {
	if len(credJSON) > 0 {
		return google.CredentialsFromJSON(ctx, credJSON, cloudPlatformScope)
	}
	return google.FindDefaultCredentials(ctx, cloudPlatformScope)
}

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
	ts, ok := googleTokenSources[fp]
	googleTokenMu.Unlock()
	if ok {
		return ts, nil
	}

	// singleflight.Do, not googleTokenMu, guards the actual resolution:
	// concurrent calls for THIS fingerprint share one resolution, but a
	// call for a DIFFERENT fingerprint runs immediately rather than
	// waiting on this one's ambient-credential round trip.
	v, err, _ := googleTokenGroup.Do(fp, func() (any, error) {
		rctx := context.WithoutCancel(ctx)
		creds, err := resolveGoogleCredentials(rctx, credJSON)
		if err != nil {
			return nil, err
		}
		ts := oauth2TokenSource{ts: creds.TokenSource}
		googleTokenMu.Lock()
		googleTokenSources[fp] = ts
		googleTokenMu.Unlock()
		return ts, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(TokenSource), nil
}

// PruneGoogleTokenSources removes every cached token source whose
// fingerprint is not in keep. Called once per registry rebuild with the
// fingerprints of every Google credential currently configured (see
// Build), so a credential that was rotated away — whose fingerprint
// changes the moment the stored ciphertext changes — doesn't sit in the
// cache forever; without this, the cache is bounded by the number of
// distinct credential VALUES ever configured across the process's whole
// lifetime, not the number of providers configured right now.
func PruneGoogleTokenSources(keep map[string]bool) {
	googleTokenMu.Lock()
	defer googleTokenMu.Unlock()
	for fp := range googleTokenSources {
		if !keep[fp] {
			delete(googleTokenSources, fp)
		}
	}
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
