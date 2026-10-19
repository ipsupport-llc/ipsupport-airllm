package openai

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"math"
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
	if req.EncodingFormat != "float" {
		t.Errorf("EncodingFormat = %q, want float", req.EncodingFormat)
	}
}

func TestDecodeEmbeddingsRequestDefaultEncodingFormatIsFloat(t *testing.T) {
	req, err := DecodeEmbeddingsRequest(strings.NewReader(`{"model":"m","input":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	if req.EncodingFormat != "float" {
		t.Errorf("EncodingFormat = %q, want float (the unset default)", req.EncodingFormat)
	}
}

// TestDecodeEmbeddingsRequestAcceptsBase64EncodingFormat proves base64 is
// accepted, not refused: the official openai-node SDK requests base64 by
// default on every call that doesn't set encoding_format itself, so
// refusing it would make the endpoint fail on that SDK's default path.
func TestDecodeEmbeddingsRequestAcceptsBase64EncodingFormat(t *testing.T) {
	req, err := DecodeEmbeddingsRequest(strings.NewReader(`{"model":"m","input":"hi","encoding_format":"base64"}`))
	if err != nil {
		t.Fatalf("encoding_format=base64 must be accepted: %v", err)
	}
	if req.EncodingFormat != "base64" {
		t.Errorf("EncodingFormat = %q, want base64", req.EncodingFormat)
	}
}

// TestDecodeEmbeddingsRequestRejectsUnsupportedEncodingFormat proves a
// request for an encoding this gateway can't produce is refused rather
// than silently answered with float vectors anyway.
func TestDecodeEmbeddingsRequestRejectsUnsupportedEncodingFormat(t *testing.T) {
	if _, err := DecodeEmbeddingsRequest(strings.NewReader(`{"model":"m","input":"hi","encoding_format":"int8"}`)); err == nil {
		t.Fatal("expected an error for encoding_format=int8")
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
	}, "float")
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

// TestMarshalEmbeddingsResponseBase64 proves encodingFormat="base64"
// answers each vector as a base64 string of little-endian float32 bytes —
// what the official OpenAI SDKs send by default and decode on receipt
// (openai-node's toFloat32Array, openai-python's numpy.frombuffer path) —
// rather than a JSON float array.
func TestMarshalEmbeddingsResponseBase64(t *testing.T) {
	b, err := MarshalEmbeddingsResponse(llm.EmbeddingsResponse{
		Model:        "m",
		Data:         [][]float64{{1, -2.5}},
		PromptTokens: 3,
	}, "base64")
	if err != nil {
		t.Fatal(err)
	}
	var w struct {
		Data []struct {
			Embedding string `json:"embedding"`
			Index     int    `json:"index"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &w); err != nil {
		t.Fatalf("response is not valid JSON with a string embedding field: %v (%s)", err, b)
	}
	if len(w.Data) != 1 {
		t.Fatalf("len(Data) = %d, want 1", len(w.Data))
	}
	raw, err := base64.StdEncoding.DecodeString(w.Data[0].Embedding)
	if err != nil {
		t.Fatalf("embedding is not valid base64: %v", err)
	}
	if len(raw) != 8 {
		t.Fatalf("decoded bytes = %d, want 8 (two float32s)", len(raw))
	}
	got := []float32{
		math.Float32frombits(binary.LittleEndian.Uint32(raw[0:4])),
		math.Float32frombits(binary.LittleEndian.Uint32(raw[4:8])),
	}
	if got[0] != 1 || got[1] != -2.5 {
		t.Errorf("decoded floats = %v, want [1 -2.5]", got)
	}
}
