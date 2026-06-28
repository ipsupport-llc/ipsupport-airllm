package secrets

import (
	"bytes"
	"crypto/sha256"
	"testing"
)

func newTestSealer(t *testing.T) *Sealer {
	t.Helper()
	k := sha256.Sum256([]byte("test-key"))
	s, err := New(k[:])
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func TestSealOpenRoundTrip(t *testing.T) {
	s := newTestSealer(t)
	pt := []byte("sk-super-secret-123")

	blob, err := s.Seal(pt)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if bytes.Contains(blob, pt) {
		t.Fatal("plaintext leaked into ciphertext")
	}
	got, err := s.Open(blob)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(got, pt) {
		t.Fatalf("round-trip mismatch: %q != %q", got, pt)
	}
}

func TestOpenRejectsTamper(t *testing.T) {
	s := newTestSealer(t)
	blob, _ := s.Seal([]byte("secret"))
	blob[len(blob)-1] ^= 0x01
	if _, err := s.Open(blob); err == nil {
		t.Fatal("expected authentication failure on tampered ciphertext")
	}
}

func TestNewRejectsBadKeyLength(t *testing.T) {
	if _, err := New([]byte("too-short")); err == nil {
		t.Fatal("expected error for non-32-byte key")
	}
}
