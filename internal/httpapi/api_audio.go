package httpapi

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/audio"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/dlp"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/ledger"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/llm"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/providers"
)

// handleAudioTranscriptions implements POST /v1/audio/transcriptions:
// multipart upload, alias-routed like chat, batch (no streaming). Response
// format is always {"text": "..."} in v1 regardless of what the client
// requests — see the design spec's out-of-scope list.
func (s *Server) handleAudioTranscriptions(w http.ResponseWriter, r *http.Request) {
	ak, _ := keyFromContext(r.Context())
	start := time.Now()
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeProtocolError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid multipart body: "+err.Error())
		return
	}
	model := r.FormValue("model")
	if model == "" {
		writeProtocolError(w, r, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}
	if !ak.Policy.Allows(model) {
		writeProtocolError(w, r, http.StatusForbidden, "permission_error", "model not permitted for this key: "+model)
		return
	}
	file, hdr, err := r.FormFile("file")
	if err != nil {
		writeProtocolError(w, r, http.StatusBadRequest, "invalid_request_error", "file is required")
		return
	}
	defer file.Close()
	audioBytes, err := io.ReadAll(file)
	if err != nil {
		writeProtocolError(w, r, http.StatusBadRequest, "invalid_request_error", "failed to read file")
		return
	}

	plan, err := s.router.Resolve(r.Context(), model, ak.Policy.AllowPassthrough)
	if err != nil {
		writeProtocolError(w, r, http.StatusNotFound, "invalid_request_error", err.Error())
		return
	}
	if msg, denied := s.limitDenied(r.Context(), ak); denied {
		s.metrics.IncRateLimited("usage_limit")
		writeProtocolError(w, r, http.StatusTooManyRequests, "rate_limit_error", msg)
		return
	}

	reg := s.reg()
	var resp audio.TranscriptionResponse
	var target string
	var upstreamModel string
	var callErr error
	succeeded := false
	for _, t := range plan.Ordered(s.router.NextRR(plan.Alias), s.freeFunc(reg)) {
		e, ok := reg.Get(t.Provider)
		if !ok {
			callErr = fmt.Errorf("provider %q not registered", t.Provider)
			continue
		}
		tr, ok := e.Provider.(providers.Transcriber)
		if !ok {
			callErr = &providers.Error{Status: http.StatusBadRequest, Retryable: false, Message: "provider " + t.Provider + " does not support transcription"}
			continue
		}
		if !e.Acquire() {
			callErr = errAllBusy
			continue
		}
		resp, callErr = tr.Transcribe(r.Context(), audio.TranscriptionRequest{
			Model: t.UpstreamModel, Audio: audioBytes, Filename: hdr.Filename,
			Language: r.FormValue("language"), Prompt: r.FormValue("prompt"),
		})
		e.Release()
		target, upstreamModel = t.Provider, t.UpstreamModel
		if callErr == nil {
			succeeded = true
			break
		}
		if !providers.IsRetryable(callErr) {
			break
		}
	}

	entry := ledger.Entry{
		KeyID: ak.KeyID, UserID: ak.UserID, Alias: model, ProviderName: target, UpstreamModel: upstreamModel,
		IngressProtocol: "openai", UpstreamProtocol: "openai", LatencyMS: time.Since(start).Milliseconds(),
	}

	if !succeeded {
		if callErr == nil {
			callErr = errAllBusy
		}
		code, typ := classifyUpstreamErr(callErr)
		if pe, ok := callErr.(*providers.Error); ok && !pe.Retryable {
			code = pe.Status
		}
		if code == http.StatusTooManyRequests {
			s.metrics.IncRateLimited("provider_busy")
		}
		entry.Status = code
		entry.ErrorMsg = callErr.Error()
		s.finalizeAudioUsage(r.Context(), entry, ak.KeyID, 0, 0, 0)
		writeProtocolError(w, r, code, typ, callErr.Error())
		return
	}

	audioSeconds := int64(math.Round(resp.DurationSeconds))
	costMicro := s.pricing.AudioCostMicroUSD(target, upstreamModel, resp.DurationSeconds)

	redactedText := resp.Text
	var findings []dlp.Finding
	if plan.DLPAudioScan {
		var blocked bool
		var msg string
		blocked, msg, findings, redactedText = s.dlpScanText(r.Context(), ak, "openai", model, resp.Text)
		if blocked {
			entry.Status = http.StatusBadRequest
			entry.ErrorMsg = msg
			s.finalizeAudioUsage(r.Context(), entry, ak.KeyID, costMicro, audioSeconds, 0)
			writeProtocolError(w, r, http.StatusBadRequest, "invalid_request_error", msg)
			return
		}
	}

	entry.Status = http.StatusOK
	s.finalizeAudioUsage(r.Context(), entry, ak.KeyID, costMicro, audioSeconds, 0)

	dlpRes := dlpResult{}
	if len(findings) > 0 {
		dlpRes = dlpResult{Findings: findings, HadIncident: true}
	}
	s.enqueueCapture(ak, "openai", model, target, upstreamModel, http.StatusOK, 0, 0, float64(costMicro)/1e6, dlpRes, nil, redactedText)

	writeJSON(w, http.StatusOK, map[string]string{"text": redactedText})
}

// handleAudioSpeech implements POST /v1/audio/speech: JSON in, raw audio
// bytes out, batch (no streaming).
func (s *Server) handleAudioSpeech(w http.ResponseWriter, r *http.Request) {
	ak, _ := keyFromContext(r.Context())
	start := time.Now()
	var body struct {
		Model          string `json:"model"`
		Input          string `json:"input"`
		Voice          string `json:"voice"`
		ResponseFormat string `json:"response_format"`
	}
	// Plain decoding, NOT the control-plane decodeJSON helper: that helper
	// sets DisallowUnknownFields, which would reject valid OpenAI-SDK
	// requests carrying documented fields this v1 doesn't use yet (e.g.
	// "speed", "instructions") — better to ignore an unsupported knob than
	// hard-reject an otherwise-compatible client.
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeProtocolError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid JSON body: "+err.Error())
		return
	}
	if body.Model == "" || body.Input == "" {
		writeProtocolError(w, r, http.StatusBadRequest, "invalid_request_error", "model and input are required")
		return
	}
	if !ak.Policy.Allows(body.Model) {
		writeProtocolError(w, r, http.StatusForbidden, "permission_error", "model not permitted for this key: "+body.Model)
		return
	}

	plan, err := s.router.Resolve(r.Context(), body.Model, ak.Policy.AllowPassthrough)
	if err != nil {
		writeProtocolError(w, r, http.StatusNotFound, "invalid_request_error", err.Error())
		return
	}
	if msg, denied := s.limitDenied(r.Context(), ak); denied {
		s.metrics.IncRateLimited("usage_limit")
		writeProtocolError(w, r, http.StatusTooManyRequests, "rate_limit_error", msg)
		return
	}

	redactedInput := body.Input
	var findings []dlp.Finding
	if plan.DLPAudioScan {
		var blocked bool
		var msg string
		blocked, msg, findings, redactedInput = s.dlpScanText(r.Context(), ak, "openai", body.Model, body.Input)
		if blocked {
			writeProtocolError(w, r, http.StatusBadRequest, "invalid_request_error", msg)
			return
		}
	}

	reg := s.reg()
	var resp audio.SpeechResponse
	var target string
	var upstreamModel string
	var callErr error
	succeeded := false
	for _, t := range plan.Ordered(s.router.NextRR(plan.Alias), s.freeFunc(reg)) {
		e, ok := reg.Get(t.Provider)
		if !ok {
			callErr = fmt.Errorf("provider %q not registered", t.Provider)
			continue
		}
		sy, ok := e.Provider.(providers.Synthesizer)
		if !ok {
			callErr = &providers.Error{Status: http.StatusBadRequest, Retryable: false, Message: "provider " + t.Provider + " does not support speech synthesis"}
			continue
		}
		if !e.Acquire() {
			callErr = errAllBusy
			continue
		}
		resp, callErr = sy.Synthesize(r.Context(), audio.SpeechRequest{
			Model: t.UpstreamModel, Input: redactedInput, Voice: body.Voice, ResponseFormat: body.ResponseFormat,
		})
		e.Release()
		target, upstreamModel = t.Provider, t.UpstreamModel
		if callErr == nil {
			succeeded = true
			break
		}
		if !providers.IsRetryable(callErr) {
			break
		}
	}

	entry := ledger.Entry{
		KeyID: ak.KeyID, UserID: ak.UserID, Alias: body.Model, ProviderName: target, UpstreamModel: upstreamModel,
		IngressProtocol: "openai", UpstreamProtocol: "openai", LatencyMS: time.Since(start).Milliseconds(),
	}

	if !succeeded {
		if callErr == nil {
			callErr = errAllBusy
		}
		code, typ := classifyUpstreamErr(callErr)
		if pe, ok := callErr.(*providers.Error); ok && !pe.Retryable {
			code = pe.Status
		}
		if code == http.StatusTooManyRequests {
			s.metrics.IncRateLimited("provider_busy")
		}
		entry.Status = code
		entry.ErrorMsg = callErr.Error()
		s.finalizeAudioUsage(r.Context(), entry, ak.KeyID, 0, 0, 0)
		writeProtocolError(w, r, code, typ, callErr.Error())
		return
	}

	ttsChars := int64(utf8.RuneCountInString(redactedInput))
	costMicro := s.pricing.TTSCostMicroUSD(target, upstreamModel, utf8.RuneCountInString(redactedInput))
	entry.Status = http.StatusOK
	s.finalizeAudioUsage(r.Context(), entry, ak.KeyID, costMicro, 0, ttsChars)

	dlpRes := dlpResult{}
	if len(findings) > 0 {
		dlpRes = dlpResult{
			Findings:        findings,
			MsgFindings:     [][]dlp.Finding{findings},
			HadIncident:     true,
			AlreadyRedacted: redactedInput != body.Input,
		}
	}
	s.enqueueCapture(ak, "openai", body.Model, target, upstreamModel, http.StatusOK, 0, 0, float64(costMicro)/1e6, dlpRes,
		[]llm.Message{{Role: "user", Content: redactedInput}}, "")

	if resp.ContentType != "" {
		w.Header().Set("Content-Type", resp.ContentType)
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(resp.Audio)
}
