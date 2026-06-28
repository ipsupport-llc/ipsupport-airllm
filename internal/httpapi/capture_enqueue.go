package httpapi

import (
	"encoding/json"

	"github.com/rromenskyi/ipsupport-airllm/internal/capture"
	"github.com/rromenskyi/ipsupport-airllm/internal/llm"
)

// captureBody serializes request messages and the response text into a compact
// JSON blob that is then sealed and written to the blob store.
func captureBody(msgs []llm.Message, response string) []byte {
	payload := struct {
		Messages []llm.Message `json:"messages"`
		Response string        `json:"response"`
	}{
		Messages: msgs,
		Response: response,
	}
	b, _ := json.Marshal(payload)
	return b
}

// enqueueCapture builds and enqueues a capture.Record if the pipeline is
// configured. It is called after finalizeUsage so all usage fields are known.
func (s *Server) enqueueCapture(
	ak authedKey,
	ingress, alias, provider, upstreamModel string,
	status, promptTokens, completionTokens int,
	costUSD float64,
	dlpRes dlpResult,
	body []byte,
) {
	pl := s.capturePl
	if pl == nil {
		return
	}
	pl.Enqueue(capture.Record{
		KeyID:            ak.KeyID,
		UserID:           ak.UserID,
		Ingress:          ingress,
		Alias:            alias,
		Provider:         provider,
		UpstreamModel:    upstreamModel,
		Status:           status,
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		CostUSD:          costUSD,
		Detected:         dlpRes.Findings,
		HadIncident:      dlpRes.HadIncident,
		Body:             body,
	})
}
