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

// maxUnbiasedByte is the largest byte value (exclusive) rejection sampling
// keeps: 256 isn't a multiple of len(alphabet) (62), so mapping every byte
// via %len(alphabet) made the first 256%62=8 alphabet characters ~25% more
// likely than the rest. Bytes at or above this threshold are discarded and
// redrawn instead, so every character has exactly 256/62=4 equally likely
// byte values mapping to it.
const maxUnbiasedByte = (256 / len(alphabet)) * len(alphabet)

// unbiasedChar maps a random byte to an alphabet character, or reports
// false when b would bias the distribution — the caller must draw another
// byte instead of using this one.
func unbiasedChar(b byte) (c byte, ok bool) {
	if int(b) >= maxUnbiasedByte {
		return 0, false
	}
	return alphabet[int(b)%len(alphabet)], true
}

func randString(n int) (string, error) {
	out := make([]byte, 0, n)
	var scratch [64]byte
	for len(out) < n {
		if _, err := rand.Read(scratch[:]); err != nil {
			return "", err
		}
		for _, b := range scratch {
			if len(out) == n {
				break
			}
			if c, ok := unbiasedChar(b); ok {
				out = append(out, c)
			}
		}
	}
	return string(out), nil
}
