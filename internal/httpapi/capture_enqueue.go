package httpapi

import (
	"encoding/json"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/capture"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/dlp"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/llm"
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

// redactForCapture returns a copy of msgs masked for a capture stored as
// redacted. Content uses the enforcement pass's per-message findings when it
// produced some; otherwise — DLP disabled or off, or nothing found — it is
// scanned here with the deterministic patterns, so a capture flagged
// redacted never holds a secret just because enforcement was off. Tool-call
// arguments are always scanned here, masked as valid JSON.
func redactForCapture(msgs []llm.Message, msgFindings [][]dlp.Finding, ps dlp.PatternSet) []llm.Message {
	out := make([]llm.Message, len(msgs))
	copy(out, msgs)
	for i := range out {
		var fs []dlp.Finding
		if msgFindings != nil {
			if i < len(msgFindings) {
				fs = msgFindings[i]
			}
		} else if out[i].Content != "" {
			fs = dlp.ScanWith(out[i].Content, ps)
		}
		if len(fs) > 0 {
			out[i].Content = dlp.Redact(out[i].Content, fs)
		}
		_, out[i].ToolCalls, _ = scanToolCallArgs(out[i].ToolCalls, ps)
	}
	return out
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
	if !capCfg.Enabled {
		return
	}
	// When DLP action="redact", messages are already masked in place; re-applying
	// the original byte offsets would corrupt the stored body (label markers are
	// longer than the redacted spans). The capture layer redacts only when it
	// must itself (action≠redact, or enforcement did not run).
	needRedact := capCfg.Redact && !dlpRes.AlreadyRedacted
	bodyMsgs := msgs
	if needRedact {
		bodyMsgs = redactForCapture(msgs, dlpRes.MsgFindings, layer1Patterns(s.dlpCfg()))
	}

	// Raw training window: when enabled, attach an un-redacted copy so the
	// flywheel can re-scan byte-aligned text. When DLP redacted in place, msgs
	// are already masked, so use the originals preserved by dlpEnforce. The
	// pipeline seals the raw copy separately; the sweeper deletes it after the TTL.
	var rawBody []byte
	if capCfg.RawTraining {
		rawSource := msgs
		if dlpRes.OriginalMessages != nil {
			rawSource = dlpRes.OriginalMessages
		}
		rawBody = captureBody(rawSource, response, false, nil)
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
		ModelVersion:     dlp.DetectorVersion,
		Detected:         dlpRes.Findings,
		HadIncident:      dlpRes.HadIncident,
		Body:             captureBody(bodyMsgs, response, false, nil),
		RawBody:          rawBody,
		Redacted:         capCfg.Redact,
	})
}
