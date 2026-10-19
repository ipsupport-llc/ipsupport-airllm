package openai

import (
	"strings"
	"testing"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/llm"
)

func TestDecodeEmbeddingsRequestSingleString(t *testing.T) {
	req, err := DecodeEmbeddingsRequest(strings.NewReader(`{"model":"m","input":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	if req.Model != "m" || len(req.Input) != 1 || req.Input[0] != "hello" {
		t.Errorf("req = %+v", req)
	}
}

func TestDecodeEmbeddingsRequestArrayOfStrings(t *testing.T) {
	req, err := DecodeEmbeddingsRequest(strings.NewReader(`{"model":"m","input":["a","b","c"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Input) != 3 || req.Input[0] != "a" || req.Input[2] != "c" {
		t.Errorf("req.Input = %v", req.Input)
	}
}

func TestDecodeEmbeddingsRequestRejectsMissingModel(t *testing.T) {
	if _, err := DecodeEmbeddingsRequest(strings.NewReader(`{"input":"hi"}`)); err == nil {
		t.Fatal("expected an error for a missing model")
	}
}

func TestDecodeEmbeddingsRequestRejectsMissingInput(t *testing.T) {
	if _, err := DecodeEmbeddingsRequest(strings.NewReader(`{"model":"m"}`)); err == nil {
		t.Fatal("expected an error for missing input")
	}
}

func TestDecodeEmbeddingsRequestRejectsEmptyArray(t *testing.T) {
	if _, err := DecodeEmbeddingsRequest(strings.NewReader(`{"model":"m","input":[]}`)); err == nil {
		t.Fatal("expected an error for an empty input array")
	}
}

func TestDecodeEmbeddingsRequestRejectsUnsupportedInputShape(t *testing.T) {
	// Token-id array input (OpenAI's other supported shape) is not accepted.
	if _, err := DecodeEmbeddingsRequest(strings.NewReader(`{"model":"m","input":[1,2,3]}`)); err == nil {
		t.Fatal("expected an error for token-id array input")
	}
}

func TestMarshalEmbeddingsResponse(t *testing.T) {
	b, err := MarshalEmbeddingsResponse(llm.EmbeddingsResponse{
		Model:        "m",
		Data:         [][]float64{{0.1, 0.2}, {0.3, 0.4}},
		PromptTokens: 7,
	})
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{
		`"object":"list"`, `"model":"m"`,
		`"embedding":[0.1,0.2]`, `"index":0`,
		`"embedding":[0.3,0.4]`, `"index":1`,
		`"prompt_tokens":7`, `"total_tokens":7`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("response %s does not contain %s", s, want)
		}
	}
	if strings.Contains(s, "completion_tokens") {
		t.Errorf("embeddings usage must not carry completion_tokens (not part of the wire shape): %s", s)
	}
}
