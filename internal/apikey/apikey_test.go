package apikey

import (
	"strings"
	"testing"
)

func TestGenerateFormat(t *testing.T) {
	k, err := Generate("dev")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !strings.HasPrefix(k.Token, "air_dev_") {
		t.Errorf("token %q missing env prefix", k.Token)
	}
	if len(k.Hash) != 64 {
		t.Errorf("hash len = %d, want 64", len(k.Hash))
	}
	if k.Last4 != k.Token[len(k.Token)-4:] {
		t.Errorf("last4 %q does not match token tail", k.Last4)
	}
	if !strings.HasPrefix(k.Token, k.Prefix) {
		t.Errorf("prefix %q is not a prefix of token", k.Prefix)
	}
}

func TestGenerateUnique(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		k, err := Generate("dev")
		if err != nil {
			t.Fatal(err)
		}
		if seen[k.Token] {
			t.Fatalf("duplicate token generated: %q", k.Token)
		}
		seen[k.Token] = true
	}
}

func TestHashStable(t *testing.T) {
	const tok = "air_dev_example"
	if Hash(tok) != Hash(tok) {
		t.Fatal("Hash is not stable")
	}
	if Hash(tok) == Hash(tok+"x") {
		t.Fatal("Hash collision on distinct tokens")
	}
	if Describe(tok).Hash != Hash(tok) {
		t.Fatal("Describe hash mismatch")
	}
}
