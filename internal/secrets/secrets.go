// Package secrets seals and opens provider credentials with AES-256-GCM. The
// master key comes from the environment; ciphertext (nonce||ct) is stored in
// Postgres. Plaintext keys are never logged or returned over the API.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"io"
)

// Sealer encrypts and decrypts small secrets.
type Sealer struct {
	gcm cipher.AEAD
}

// New builds a Sealer from a 32-byte master key (AES-256).
func New(masterKey []byte) (*Sealer, error) {
	if len(masterKey) != 32 {
		return nil, errors.New("master key must be 32 bytes")
	}
	block, err := aes.NewCipher(masterKey)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Sealer{gcm: gcm}, nil
}

// Seal returns nonce||ciphertext for plaintext.
func (s *Sealer) Seal(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, s.gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return s.gcm.Seal(nonce, nonce, plaintext, nil), nil
}

// Open recovers plaintext from nonce||ciphertext.
func (s *Sealer) Open(blob []byte) ([]byte, error) {
	ns := s.gcm.NonceSize()
	if len(blob) < ns {
		return nil, errors.New("ciphertext too short")
	}
	return s.gcm.Open(nil, blob[:ns], blob[ns:], nil)
}
