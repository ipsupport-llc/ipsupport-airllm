package openai

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/llm"
)

type embeddingsRequestWire struct {
	Model          string          `json:"model"`
	Input          json.RawMessage `json:"input"`
	Dimensions     *int            `json:"dimensions,omitempty"`
	EncodingFormat string          `json:"encoding_format,omitempty"`
}

// DecodeEmbeddingsRequest parses an OpenAI embeddings request body. input
// may be a single string or an array of strings; OpenAI's other supported
// shape, an array of token ids, is not accepted — nothing this gateway
// talks to sends pre-tokenized input. encoding_format is "float" (the
// unset default) or "base64" — the official OpenAI SDKs request base64 on
// every call that doesn't set the field itself, for transfer size, and
// decode it client-side; anything else is refused rather than silently
// answered with floats anyway.
func DecodeEmbeddingsRequest(r io.Reader) (llm.EmbeddingsRequest, error) {
	var w embeddingsRequestWire
	if err := json.NewDecoder(r).Decode(&w); err != nil {
		return llm.EmbeddingsRequest{}, err
	}
	if w.Model == "" {
		return llm.EmbeddingsRequest{}, errors.New("model is required")
	}
	encodingFormat := w.EncodingFormat
	if encodingFormat == "" {
		encodingFormat = "float"
	}
	if encodingFormat != "float" && encodingFormat != "base64" {
		return llm.EmbeddingsRequest{}, fmt.Errorf("encoding_format %q is not supported; use \"float\" or \"base64\"", w.EncodingFormat)
	}
	input, err := decodeEmbeddingsInput(w.Input)
	if err != nil {
		return llm.EmbeddingsRequest{}, err
	}
	if len(input) == 0 {
		return llm.EmbeddingsRequest{}, errors.New("input is required")
	}
	return llm.EmbeddingsRequest{Model: w.Model, Input: input, Dimensions: w.Dimensions, EncodingFormat: encodingFormat}, nil
}

func decodeEmbeddingsInput(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		if single == "" {
			return nil, nil
		}
		return []string{single}, nil
	}
	var multi []string
	if err := json.Unmarshal(raw, &multi); err == nil {
		for _, s := range multi {
			if s == "" {
				return nil, errors.New("input must not contain an empty string")
			}
		}
		return multi, nil
	}
	return nil, errors.New("input must be a string or an array of strings")
}

// embeddingWire's Embedding field is []float64 for encodingFormat="float"
// or a base64 string for "base64" — the same type-by-request-field
// behavior the real OpenAI API has, which is why it is `any` rather than a
// fixed type.
type embeddingWire struct {
	Object    string `json:"object"`
	Embedding any    `json:"embedding"`
	Index     int    `json:"index"`
}

type embeddingsUsageWire struct {
	PromptTokens int `json:"prompt_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

type embeddingsResponseWire struct {
	Object string              `json:"object"`
	Data   []embeddingWire     `json:"data"`
	Model  string              `json:"model"`
	Usage  embeddingsUsageWire `json:"usage"`
}

// MarshalEmbeddingsResponse renders an llm.EmbeddingsResponse as OpenAI
// embeddings JSON bytes. Embeddings have no completion tokens, so usage
// carries only prompt_tokens/total_tokens (equal to each other) — never a
// completion_tokens field llm.Usage's own marshaller would add.
//
// encodingFormat is the caller's EmbeddingsRequest.EncodingFormat
// ("float" or "base64"), not a provider concern: every provider call
// decodes a plain float array from upstream regardless of what the
// gateway's own client asked for, so this is the one place the format the
// client actually requested is applied.
func MarshalEmbeddingsResponse(resp llm.EmbeddingsResponse, encodingFormat string) ([]byte, error) {
	out := embeddingsResponseWire{
		Object: "list",
		Model:  resp.Model,
		Usage:  embeddingsUsageWire{PromptTokens: resp.PromptTokens, TotalTokens: resp.PromptTokens},
	}
	out.Data = make([]embeddingWire, len(resp.Data))
	for i, vec := range resp.Data {
		var embedding any = vec
		if encodingFormat == "base64" {
			embedding = encodeFloat32Base64(vec)
		}
		out.Data[i] = embeddingWire{Object: "embedding", Embedding: embedding, Index: i}
	}
	return json.Marshal(out)
}

// encodeFloat32Base64 renders vec as OpenAI's base64 embedding encoding:
// each value narrowed to float32 (the API's own on-wire precision for this
// format), packed little-endian, then base64-standard-encoded — the exact
// shape openai-node's toFloat32Array and openai-python's numpy.frombuffer
// decode on receipt.
func encodeFloat32Base64(vec []float64) string {
	buf := make([]byte, 4*len(vec))
	for i, f := range vec {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(float32(f)))
	}
	return base64.StdEncoding.EncodeToString(buf)
}
