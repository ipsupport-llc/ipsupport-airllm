package openai

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"

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
// talks to sends pre-tokenized input. encoding_format other than "float"
// (including the unset default) is refused rather than silently ignored:
// some client SDKs default to "base64" and decode the reply as such, so
// answering those with float arrays would hand them corrupt data instead
// of a clear error.
func DecodeEmbeddingsRequest(r io.Reader) (llm.EmbeddingsRequest, error) {
	var w embeddingsRequestWire
	if err := json.NewDecoder(r).Decode(&w); err != nil {
		return llm.EmbeddingsRequest{}, err
	}
	if w.Model == "" {
		return llm.EmbeddingsRequest{}, errors.New("model is required")
	}
	if w.EncodingFormat != "" && w.EncodingFormat != "float" {
		return llm.EmbeddingsRequest{}, fmt.Errorf("encoding_format %q is not supported; only \"float\" is", w.EncodingFormat)
	}
	input, err := decodeEmbeddingsInput(w.Input)
	if err != nil {
		return llm.EmbeddingsRequest{}, err
	}
	if len(input) == 0 {
		return llm.EmbeddingsRequest{}, errors.New("input is required")
	}
	return llm.EmbeddingsRequest{Model: w.Model, Input: input, Dimensions: w.Dimensions}, nil
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

type embeddingWire struct {
	Object    string    `json:"object"`
	Embedding []float64 `json:"embedding"`
	Index     int       `json:"index"`
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
func MarshalEmbeddingsResponse(resp llm.EmbeddingsResponse) ([]byte, error) {
	out := embeddingsResponseWire{
		Object: "list",
		Model:  resp.Model,
		Usage:  embeddingsUsageWire{PromptTokens: resp.PromptTokens, TotalTokens: resp.PromptTokens},
	}
	out.Data = make([]embeddingWire, len(resp.Data))
	for i, vec := range resp.Data {
		out.Data[i] = embeddingWire{Object: "embedding", Embedding: vec, Index: i}
	}
	return json.Marshal(out)
}
