package httpapi

import (
	"encoding/json"

	"github.com/rromenskyi/ipsupport-airllm/internal/capture"
	"github.com/rromenskyi/ipsupport-airllm/internal/dlp"
	"github.com/rromenskyi/ipsupport-airllm/internal/llm"
)

// captureBody serializes request messages and the response text into a compact
// JSON blob for storage. When redact=true and per-message findings are
// provided, it applies DLP redaction to a copy of msgs before serializing,
// ensuring the stored body never contains raw secrets regardless of the DLP
// enforcement action (flag vs redact).
func captureBody(msgs []llm.Message, response string, redact bool, msgFindings [][]dlp.Finding) []byte {
	if redact && len(msgFindings) > 0 {
		redacted := make([]llm.Message, len(msgs))
		copy(redacted, msgs)
		for i, findings := range msgFindings {
			if i < len(redacted) && len(findings) > 0 {
				redacted[i].Content = dlp.Redact(redacted[i].Content, findings)
			}
		}
		msgs = redacted
	}
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
// The capture config (including Redact) is snapshotted here so the DB flag
// matches the body actually stored.
func (s *Server) enqueueCapture(
	ak authedKey,
	ingress, alias, provider, upstreamModel string,
	status, promptTokens, completionTokens int,
	costUSD float64,
	dlpRes dlpResult,
	msgs []llm.Message,
	response string,
) {
	pl := s.capturePl
	if pl == nil {
		return
	}
	capCfg := s.captureCfg()
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
		Body:             captureBody(msgs, response, capCfg.Redact, dlpRes.MsgFindings),
		Redacted:         capCfg.Redact,
	})
}
