package openai

import (
	"strings"
	"testing"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/llm"
)

func TestEncodeEmbeddingsRequest(t *testing.T) {
	b, err := EncodeEmbeddingsRequest(llm.EmbeddingsRequest{Model: "m", Input: []string{"a", "b"}})
	if err != nil {
		t.Fatal(err)
	}
	if s := string(b); s != `{"model":"m","input":["a","b"]}` {
		t.Errorf("got %s", s)
	}
}

// TestDecodeEmbeddingsResponseOrdersByIndex proves a vendor that returns
// entries out of order is still placed correctly, keyed by the index field
// rather than array position.
func TestDecodeEmbeddingsResponseOrdersByIndex(t *testing.T) {
	body := `{"model":"m","data":[{"embedding":[9,9],"index":1},{"embedding":[1,1],"index":0}],"usage":{"prompt_tokens":5}}`
	resp, err := DecodeEmbeddingsResponse(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Data) != 2 || resp.Data[0][0] != 1 || resp.Data[1][0] != 9 {
		t.Errorf("resp.Data = %v, want index 0 -> [1,1], index 1 -> [9,9]", resp.Data)
	}
	if resp.PromptTokens != 5 {
		t.Errorf("PromptTokens = %d, want 5", resp.PromptTokens)
	}
}
