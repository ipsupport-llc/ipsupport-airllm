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

// TestUnbiasedCharDistributionIsExactlyEven is the Auth-M1 fix: mapping a
// random byte to an alphabet character via %len(alphabet) is biased unless
// 256 is an exact multiple of the alphabet length (62 here; 256/62 leaves a
// remainder of 8), so some characters land MORE often than others. This
// test is exhaustive and deterministic — it tries every possible byte value
// (no real randomness involved) and asserts every alphabet character is
// reachable from exactly the same number of byte values.
func TestUnbiasedCharDistributionIsExactlyEven(t *testing.T) {
	counts := make(map[byte]int, len(alphabet))
	rejected := 0
	for b := 0; b <= 255; b++ {
		c, ok := unbiasedChar(byte(b))
		if !ok {
			rejected++
			continue
		}
		counts[c]++
	}
	wantRejected := 256 - maxUnbiasedByte
	if rejected != wantRejected {
		t.Errorf("rejected %d byte values, want exactly %d (256 - maxUnbiasedByte)", rejected, wantRejected)
	}
	want := maxUnbiasedByte / len(alphabet)
	for _, c := range []byte(alphabet) {
		if counts[c] != want {
			t.Errorf("alphabet char %q is reachable from %d byte values, want exactly %d (even distribution)", c, counts[c], want)
		}
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
