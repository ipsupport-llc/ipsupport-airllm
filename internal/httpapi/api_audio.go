package httpapi

import (
	"io"
	"log/slog"
	"net/http"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/audio"
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
		writeProtocolError(w, r, http.StatusTooManyRequests, "rate_limit_error", msg)
		return
	}

	reg := s.reg()
	var resp audio.TranscriptionResponse
	var target string
	var upstreamModel string
	var callErr error
	for _, t := range plan.Ordered(s.router.NextRR(plan.Alias), s.freeFunc(reg)) {
		e, ok := reg.Get(t.Provider)
		if !ok {
			continue
		}
		tr, ok := e.Provider.(providers.Transcriber)
		if !ok {
			callErr = &providers.Error{Status: http.StatusBadRequest, Retryable: false, Message: "provider " + t.Provider + " does not support transcription"}
			continue
		}
		if !e.Acquire() {
			continue
		}
		resp, callErr = tr.Transcribe(r.Context(), audio.TranscriptionRequest{
			Model: t.UpstreamModel, Audio: audioBytes, Filename: hdr.Filename,
			Language: r.FormValue("language"), Prompt: r.FormValue("prompt"),
		})
		e.Release()
		target, upstreamModel = t.Provider, t.UpstreamModel
		if callErr == nil {
			break
		}
		if !providers.IsRetryable(callErr) {
			break
		}
	}
	if callErr != nil {
		code, typ := classifyUpstreamErr(callErr)
		if pe, ok := callErr.(*providers.Error); ok && !pe.Retryable {
			code = pe.Status
		}
		writeProtocolError(w, r, code, typ, callErr.Error())
		return
	}

	redactedText := resp.Text
	if plan.DLPAudioScan {
		var blocked bool
		var msg string
		blocked, msg, _, redactedText = s.dlpScanText(r.Context(), ak, "openai", resp.Text)
		if blocked {
			writeProtocolError(w, r, http.StatusBadRequest, "invalid_request_error", msg)
			return
		}
	}

	costMicro := s.pricing.AudioCostMicroUSD(target, upstreamModel, resp.DurationSeconds)
	s.ledger.Record(r.Context(), ledger.Entry{
		KeyID: ak.KeyID, UserID: ak.UserID, Alias: model, ProviderName: target, UpstreamModel: upstreamModel,
		IngressProtocol: "openai", UpstreamProtocol: "openai", Status: http.StatusOK, CostUSD: float64(costMicro) / 1e6,
	})
	if err := s.limiter.Add(r.Context(), ak.KeyID, 0, 0, int64(resp.DurationSeconds), 0); err != nil {
		slog.Error("limiter add failed", "err", err)
	}
	s.enqueueCapture(ak, "openai", model, target, upstreamModel, http.StatusOK, 0, 0, float64(costMicro)/1e6, dlpResult{}, nil, redactedText)

	writeJSON(w, http.StatusOK, map[string]string{"text": redactedText})
}

// handleAudioSpeech implements POST /v1/audio/speech: JSON in, raw audio
// bytes out, batch (no streaming).
func (s *Server) handleAudioSpeech(w http.ResponseWriter, r *http.Request) {
	ak, _ := keyFromContext(r.Context())
	var body struct {
		Model          string `json:"model"`
		Input          string `json:"input"`
		Voice          string `json:"voice"`
		ResponseFormat string `json:"response_format"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeProtocolError(w, r, http.StatusBadRequest, "invalid_request_error", err.Error())
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
		writeProtocolError(w, r, http.StatusTooManyRequests, "rate_limit_error", msg)
		return
	}

	redactedInput := body.Input
	if plan.DLPAudioScan {
		var blocked bool
		var msg string
		blocked, msg, _, redactedInput = s.dlpScanText(r.Context(), ak, "openai", body.Input)
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
	for _, t := range plan.Ordered(s.router.NextRR(plan.Alias), s.freeFunc(reg)) {
		e, ok := reg.Get(t.Provider)
		if !ok {
			continue
		}
		sy, ok := e.Provider.(providers.Synthesizer)
		if !ok {
			callErr = &providers.Error{Status: http.StatusBadRequest, Retryable: false, Message: "provider " + t.Provider + " does not support speech synthesis"}
			continue
		}
		if !e.Acquire() {
			continue
		}
		resp, callErr = sy.Synthesize(r.Context(), audio.SpeechRequest{
			Model: t.UpstreamModel, Input: redactedInput, Voice: body.Voice, ResponseFormat: body.ResponseFormat,
		})
		e.Release()
		target, upstreamModel = t.Provider, t.UpstreamModel
		if callErr == nil {
			break
		}
		if !providers.IsRetryable(callErr) {
			break
		}
	}
	if callErr != nil {
		code, typ := classifyUpstreamErr(callErr)
		if pe, ok := callErr.(*providers.Error); ok && !pe.Retryable {
			code = pe.Status
		}
		writeProtocolError(w, r, code, typ, callErr.Error())
		return
	}

	costMicro := s.pricing.TTSCostMicroUSD(target, upstreamModel, len(redactedInput))
	s.ledger.Record(r.Context(), ledger.Entry{
		KeyID: ak.KeyID, UserID: ak.UserID, Alias: body.Model, ProviderName: target, UpstreamModel: upstreamModel,
		IngressProtocol: "openai", UpstreamProtocol: "openai", Status: http.StatusOK, CostUSD: float64(costMicro) / 1e6,
	})
	if err := s.limiter.Add(r.Context(), ak.KeyID, 0, 0, 0, int64(len(redactedInput))); err != nil {
		slog.Error("limiter add failed", "err", err)
	}
	s.enqueueCapture(ak, "openai", body.Model, target, upstreamModel, http.StatusOK, 0, 0, float64(costMicro)/1e6, dlpResult{},
		[]llm.Message{{Role: "user", Content: redactedInput}}, "")

	if resp.ContentType != "" {
		w.Header().Set("Content-Type", resp.ContentType)
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(resp.Audio)
}
