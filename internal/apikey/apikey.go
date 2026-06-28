// Package apikey generates and fingerprints gateway API keys. The full
// token is shown to the user once; only its sha256 hash (plus a prefix and
// last-4 for identification) is persisted.
package apikey

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

const (
	alphabet  = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	secretLen = 40
	prefixLen = 12 // "air_dev_" + first chars of the secret
)

// Key is a generated (or described) API key and its stored fingerprints.
type Key struct {
	Token  string // full secret, shown once
	Hash   string // sha256 hex of Token
	Prefix string // leading chars, safe to display/store
	Last4  string // trailing 4 chars, safe to display/store
}

// Generate mints a new key of the form "air_<env>_<random>".
func Generate(envTag string) (Key, error) {
	secret, err := randString(secretLen)
	if err != nil {
		return Key{}, err
	}
	return Describe(fmt.Sprintf("air_%s_%s", envTag, secret)), nil
}

// Describe computes the stored fingerprints for an existing token.
func Describe(token string) Key {
	prefix := token
	if len(prefix) > prefixLen {
		prefix = prefix[:prefixLen]
	}
	last4 := token
	if len(last4) > 4 {
		last4 = last4[len(last4)-4:]
	}
	return Key{Token: token, Hash: Hash(token), Prefix: prefix, Last4: last4}
}

// Hash returns the hex sha256 of a token, as stored and looked up.
func Hash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func randString(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	for i := range buf {
		buf[i] = alphabet[int(buf[i])%len(alphabet)]
	}
	return string(buf), nil
}
