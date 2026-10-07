package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/audio"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/dlp"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/ledger"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/llm"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/providers"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/routing"
)

// handleAudioTranscriptions implements POST /v1/audio/transcriptions:
// multipart upload, alias-routed like chat, batch (no streaming).
//
// language is a BCP-47 tag and alternative_languages lists further ones
// (repeated, or comma-separated; the OpenAI-style "alternative_languages[]"
// spelling works too). With response_format=verbose_json the reply carries
// text, language, confidence and duration from whichever tier served it;
// any other format gets {"text": "..."}.
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
	if msg, denied, _ := s.limitDenied(r.Context(), ak, 0); denied {
		s.metrics.IncRateLimited("usage_limit")
		writeProtocolError(w, r, http.StatusTooManyRequests, "rate_limit_error", msg)
		return
	}

	resp, res, callErr := s.runTranscribe(r.Context(), plan, audio.TranscriptionRequest{
		Audio: audioBytes, Filename: hdr.Filename,
		Language:             audio.CanonicalLanguage(r.FormValue("language")),
		AlternativeLanguages: alternativeLanguages(r),
		Prompt:               r.FormValue("prompt"),
	})
	target, upstreamModel := res.Provider, res.UpstreamModel

	entry := ledger.Entry{
		KeyID: ak.KeyID, UserID: ak.UserID, Alias: model, ProviderName: target, UpstreamModel: upstreamModel,
		IngressProtocol: "openai", UpstreamProtocol: "openai", Tier: res.Tier, Attempts: res.Attempts,
		Session: res.Session, LatencyMS: time.Since(start).Milliseconds(),
	}

	if callErr != nil {
		code, typ := classifyUpstreamErr(callErr)
		if pe, ok := callErr.(*providers.Error); ok && !pe.Retryable {
			code = pe.Status
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

	if r.FormValue("response_format") == "verbose_json" {
		writeJSON(w, http.StatusOK, verboseTranscription{
			Task: "transcribe", Text: redactedText, Language: resp.Language,
			Confidence: resp.Confidence, Duration: resp.DurationSeconds,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"text": redactedText})
}

// verboseTranscription is the verbose_json reply: OpenAI's field names, so
// its SDKs parse it, with language as a BCP-47 tag rather than OpenAI's
// English name, and a confidence OpenAI does not have (0 = the serving
// provider reported none).
type verboseTranscription struct {
	Task       string  `json:"task"`
	Text       string  `json:"text"`
	Language   string  `json:"language"`
	Confidence float64 `json:"confidence"`
	Duration   float64 `json:"duration"`
}

// alternativeLanguages collects the alternative_languages form field in any
// of the spellings a multipart client produces, canonical and without
// duplicates.
func alternativeLanguages(r *http.Request) []string {
	if r.MultipartForm == nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, field := range []string{"alternative_languages", "alternative_languages[]"} {
		for _, v := range r.MultipartForm.Value[field] {
			for _, tag := range strings.Split(v, ",") {
				if tag = audio.CanonicalLanguage(tag); tag != "" && !seen[tag] {
					seen[tag] = true
					out = append(out, tag)
				}
			}
		}
	}
	return out
}

// handleAudioCapabilities implements GET /v1/audio/capabilities?model=:
// what the alias's first tier can do, so a client can offer its users the
// choices that tier supports. The first target of that tier which can
// transcribe answers for recognition; an alias whose first tier cannot
// transcribe has no recognition entry.
func (s *Server) handleAudioCapabilities(w http.ResponseWriter, r *http.Request) {
	ak, _ := keyFromContext(r.Context())
	model := r.URL.Query().Get("model")
	if model == "" {
		writeProtocolError(w, r, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}
	if !ak.Policy.Allows(model) {
		writeProtocolError(w, r, http.StatusForbidden, "permission_error", "model not permitted for this key: "+model)
		return
	}
	plan, err := s.router.Resolve(r.Context(), model, ak.Policy.AllowPassthrough)
	if err != nil {
		writeProtocolError(w, r, http.StatusNotFound, "invalid_request_error", err.Error())
		return
	}

	type recognition struct {
		Languages []string `json:"languages"`
	}
	out := struct {
		Model       string       `json:"model"`
		Recognition *recognition `json:"recognition,omitempty"`
	}{Model: model}
	reg := s.reg()
	for _, t := range plan.Tiers[0] {
		e, ok := reg.Get(t.Provider)
		if !ok {
			continue
		}
		if _, ok := e.Provider.(providers.Transcriber); !ok {
			continue
		}
		langs := []string{}
		if l, ok := e.Provider.(providers.RecognitionLanguageLister); ok {
			langs = l.RecognitionLanguages()
		}
		out.Recognition = &recognition{Languages: langs}
		break
	}
	writeJSON(w, http.StatusOK, out)
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
	if msg, denied, _ := s.limitDenied(r.Context(), ak, 0); denied {
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

	resp, res, callErr := s.runSynthesize(r.Context(), plan, audio.SpeechRequest{
		Input: redactedInput, Voice: body.Voice, ResponseFormat: body.ResponseFormat,
	})
	target, upstreamModel := res.Provider, res.UpstreamModel

	entry := ledger.Entry{
		KeyID: ak.KeyID, UserID: ak.UserID, Alias: body.Model, ProviderName: target, UpstreamModel: upstreamModel,
		IngressProtocol: "openai", UpstreamProtocol: "openai", Tier: res.Tier, Attempts: res.Attempts,
		Session: res.Session, LatencyMS: time.Since(start).Milliseconds(),
	}

	if callErr != nil {
		code, typ := classifyUpstreamErr(callErr)
		if pe, ok := callErr.(*providers.Error); ok && !pe.Retryable {
			code = pe.Status
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

// runTranscribe executes the plan for a transcription (see executePlan),
// skipping targets whose provider cannot transcribe.
//
// Every tier answers with the same shape: a provider that chose its own
// model by language reports it, and the result names that model so the
// ledger prices what actually ran; a provider silent on the language is
// taken to have recognised the one requested, and one that heard the
// requested language but names it without a region (the Whisper family
// does) reports the requested tag, so the language keeps its form when a
// request fails over.
func (s *Server) runTranscribe(ctx context.Context, plan *routing.Plan, req audio.TranscriptionRequest) (audio.TranscriptionResponse, execResult, error) {
	var resp audio.TranscriptionResponse
	supports := func(p providers.Provider) error {
		if _, ok := p.(providers.Transcriber); !ok {
			return &providers.Error{Status: http.StatusBadRequest, Retryable: false, Message: "provider " + p.Name() + " does not support transcription"}
		}
		return nil
	}
	res, _, err := s.executePlan(ctx, plan, supports, func(ctx context.Context, p providers.Provider, t routing.Target, _ func() bool) error {
		in := req
		in.Model = t.UpstreamModel
		in.ModelByLanguage = t.Options.RecognitionModels
		var err error
		resp, err = p.(providers.Transcriber).Transcribe(ctx, in)
		return err
	})
	if err != nil {
		return audio.TranscriptionResponse{}, res, err
	}
	if resp.Model != "" {
		res.UpstreamModel = resp.Model
	}
	if resp.Language == "" || resp.Language == audio.PrimaryLanguage(req.Language) {
		resp.Language = audio.CanonicalLanguage(req.Language)
	}
	return resp, res, nil
}

// runSynthesize executes the plan for a speech synthesis (see executePlan),
// skipping targets whose provider cannot synthesize.
func (s *Server) runSynthesize(ctx context.Context, plan *routing.Plan, req audio.SpeechRequest) (audio.SpeechResponse, execResult, error) {
	var resp audio.SpeechResponse
	supports := func(p providers.Provider) error {
		if _, ok := p.(providers.Synthesizer); !ok {
			return &providers.Error{Status: http.StatusBadRequest, Retryable: false, Message: "provider " + p.Name() + " does not support speech synthesis"}
		}
		return nil
	}
	res, _, err := s.executePlan(ctx, plan, supports, func(ctx context.Context, p providers.Provider, t routing.Target, _ func() bool) error {
		in := req
		in.Model = t.UpstreamModel
		var err error
		resp, err = p.(providers.Synthesizer).Synthesize(ctx, in)
		return err
	})
	if err != nil {
		return audio.SpeechResponse{}, res, err
	}
	return resp, res, nil
}
