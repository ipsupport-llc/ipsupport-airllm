package openai

import (
	"encoding/json"
	"io"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/llm"
)

type upstreamEmbeddingsRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

// EncodeEmbeddingsRequest renders the IR as an OpenAI embeddings request
// body for an upstream call.
func EncodeEmbeddingsRequest(req llm.EmbeddingsRequest) ([]byte, error) {
	return json.Marshal(upstreamEmbeddingsRequest{Model: req.Model, Input: req.Input})
}

type upstreamEmbedding struct {
	Embedding []float64 `json:"embedding"`
	Index     int       `json:"index"`
}

type upstreamEmbeddingsResponse struct {
	Data  []upstreamEmbedding `json:"data"`
	Model string              `json:"model"`
	Usage struct {
		PromptTokens int `json:"prompt_tokens"`
	} `json:"usage"`
}

// DecodeEmbeddingsResponse parses an upstream OpenAI embeddings response,
// placing each vector at its reported index — never assumed to equal its
// position in the data array — so a vendor that reorders results is still
// handled correctly.
func DecodeEmbeddingsResponse(r io.Reader) (llm.EmbeddingsResponse, error) {
	var w upstreamEmbeddingsResponse
	if err := json.NewDecoder(r).Decode(&w); err != nil {
		return llm.EmbeddingsResponse{}, err
	}
	out := llm.EmbeddingsResponse{Model: w.Model, PromptTokens: w.Usage.PromptTokens}
	out.Data = make([][]float64, len(w.Data))
	for _, d := range w.Data {
		if d.Index >= 0 && d.Index < len(out.Data) {
			out.Data[d.Index] = d.Embedding
		}
	}
	return out, nil
}
