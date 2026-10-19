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

func TestDecodeEmbeddingsRequestPassesThroughDimensions(t *testing.T) {
	req, err := DecodeEmbeddingsRequest(strings.NewReader(`{"model":"m","input":"hi","dimensions":256}`))
	if err != nil {
		t.Fatal(err)
	}
	if req.Dimensions == nil || *req.Dimensions != 256 {
		t.Errorf("Dimensions = %v, want 256", req.Dimensions)
	}
}

func TestDecodeEmbeddingsRequestOmittedDimensionsIsNil(t *testing.T) {
	req, err := DecodeEmbeddingsRequest(strings.NewReader(`{"model":"m","input":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	if req.Dimensions != nil {
		t.Errorf("Dimensions = %v, want nil", req.Dimensions)
	}
}

func TestDecodeEmbeddingsRequestAcceptsExplicitFloatEncodingFormat(t *testing.T) {
	req, err := DecodeEmbeddingsRequest(strings.NewReader(`{"model":"m","input":"hi","encoding_format":"float"}`))
	if err != nil {
		t.Fatalf("encoding_format=float must be accepted: %v", err)
	}
	if req.Model != "m" {
		t.Errorf("req = %+v", req)
	}
}

// TestDecodeEmbeddingsRequestRejectsUnsupportedEncodingFormat proves a
// request for base64 (or any non-float) encoding is refused rather than
// silently answered with float vectors — some client SDKs default to
// base64 and decode the reply as such, so answering with floats under a
// base64 request would hand them corrupt data instead of an error.
func TestDecodeEmbeddingsRequestRejectsUnsupportedEncodingFormat(t *testing.T) {
	if _, err := DecodeEmbeddingsRequest(strings.NewReader(`{"model":"m","input":"hi","encoding_format":"base64"}`)); err == nil {
		t.Fatal("expected an error for encoding_format=base64")
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

func TestDecodeEmbeddingsRequestRejectsEmptyStringInArray(t *testing.T) {
	if _, err := DecodeEmbeddingsRequest(strings.NewReader(`{"model":"m","input":["a",""]}`)); err == nil {
		t.Fatal("expected an error for an empty string within the input array")
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
