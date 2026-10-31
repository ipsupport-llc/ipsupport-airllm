package providers

import (
	"context"
	"testing"
	"time"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/secrets"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/store"
)

// TestBuildAppliesChatTimeoutToOpenAICompat proves Build threads its
// chatTimeout argument down to every OpenAI-compatible provider it
// constructs, rather than leaving them on the 120s built-in default.
func TestBuildAppliesChatTimeoutToOpenAICompat(t *testing.T) {
	sealer, err := secrets.New(make([]byte, 32))
	if err != nil {
		t.Fatalf("secrets.New: %v", err)
	}
	rows := []store.ProviderRow{
		{Name: "up", Kind: "openai", BaseURL: "https://example.invalid/v1", MaxConcurrency: 0, Config: []byte(`{}`)},
	}
	reg := Build(context.Background(), rows, sealer, 7*time.Second)
	entry, ok := reg.Get("up")
	if !ok {
		t.Fatal("provider \"up\" not registered")
	}
	oc, ok := entry.Provider.(*OpenAICompat)
	if !ok {
		t.Fatalf("provider is %T, want *OpenAICompat", entry.Provider)
	}
	if oc.chatTimeout != 7*time.Second {
		t.Errorf("chatTimeout = %v, want 7s", oc.chatTimeout)
	}
}
