package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/audio"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/dlp"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/ledger"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/llm"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/providers"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/routing"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/speechcache"
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
		s.finalizeAudioUsage(r.Context(), entry, ak.KeyID, 0, 0, 0, "")
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
			s.finalizeAudioUsage(r.Context(), entry, ak.KeyID, costMicro, audioSeconds, 0, "")
			writeProtocolError(w, r, http.StatusBadRequest, "invalid_request_error", msg)
			return
		}
	}

	entry.Status = http.StatusOK
	s.finalizeAudioUsage(r.Context(), entry, ak.KeyID, costMicro, audioSeconds, 0, "")

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
// transcribe answers for recognition, and the first which can synthesize
// and list its voices for synthesis: those its own voices option lists,
// else the provider's catalogue. An alias whose first tier cannot do one of
// them has no entry for it; one whose synthesizers all fail to list their
// voices answers with the failure, since an empty list would read as "no
// voices".
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
	type synthesis struct {
		Voices []audio.Voice `json:"voices"`
	}
	out := struct {
		Model       string       `json:"model"`
		Recognition *recognition `json:"recognition,omitempty"`
		Synthesis   *synthesis   `json:"synthesis,omitempty"`
	}{Model: model}
	reg := s.reg()
	var voicesErr error
	for _, t := range plan.Tiers[0] {
		e, ok := reg.Get(t.Provider)
		if !ok {
			continue
		}
		if _, ok := e.Provider.(providers.Transcriber); ok && out.Recognition == nil {
			langs := []string{}
			if l, ok := e.Provider.(providers.RecognitionLanguageLister); ok {
				langs = l.RecognitionLanguages()
			}
			out.Recognition = &recognition{Languages: langs}
		}
		if _, ok := e.Provider.(providers.Synthesizer); ok && out.Synthesis == nil {
			voices, err := targetVoices(r.Context(), e.Provider, t)
			if err != nil {
				voicesErr = err
				continue
			}
			out.Synthesis = &synthesis{Voices: voices}
		}
	}
	if out.Synthesis == nil && voicesErr != nil {
		code, typ := classifyUpstreamErr(voicesErr)
		writeProtocolError(w, r, code, typ, voicesErr.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// targetVoices is the voice catalogue of one synthesizing target: the
// canonical voices its options map, described by their entries or else by
// their names, or failing that the provider's own list; none when it has
// neither. Sorted by identifier.
func targetVoices(ctx context.Context, p providers.Provider, t routing.Target) ([]audio.Voice, error) {
	voices := []audio.Voice{}
	if len(t.Options.Voices) > 0 {
		for id, v := range t.Options.Voices {
			lang := audio.CanonicalLanguage(v.Language)
			if lang == "" {
				lang = audio.VoiceLanguage(id)
			}
			voices = append(voices, audio.Voice{ID: id, Language: lang, Gender: v.Gender})
		}
	} else if l, ok := p.(providers.VoiceLister); ok {
		listed, err := l.ListVoices(ctx)
		if err != nil {
			return nil, err
		}
		voices = append(voices, listed...)
	}
	slices.SortFunc(voices, func(a, b audio.Voice) int { return strings.Compare(a.ID, b.ID) })
	return voices, nil
}

// handleAudioSpeech implements POST /v1/audio/speech: JSON in, raw audio
// bytes out, batch (no streaming). The format defaults to WAV, whichever
// tier speaks, with the sample rate in its header; language optionally
// names the input's language for a voice whose name does not carry it.
func (s *Server) handleAudioSpeech(w http.ResponseWriter, r *http.Request) {
	ak, _ := keyFromContext(r.Context())
	start := time.Now()
	var body struct {
		Model          string `json:"model"`
		Input          string `json:"input"`
		Voice          string `json:"voice"`
		Language       string `json:"language"`
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

	format := strings.ToLower(strings.TrimSpace(body.ResponseFormat))
	if format == "" {
		format = "wav"
	}
	resp, res, callErr := s.runSynthesize(r.Context(), plan, audio.SpeechRequest{
		Input: redactedInput, Voice: body.Voice, Language: audio.CanonicalLanguage(body.Language), ResponseFormat: format,
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
		s.finalizeAudioUsage(r.Context(), entry, ak.KeyID, 0, 0, 0, "")
		writeProtocolError(w, r, code, typ, callErr.Error())
		return
	}

	// A cached reply cost the gateway nothing, but its characters still
	// count against the key's character limit, which is there to stop a
	// runaway client, not to bill it.
	ttsChars := int64(utf8.RuneCountInString(redactedInput))
	var costMicro int64
	if res.Cache != cacheHit {
		costMicro = s.pricing.TTSCostMicroUSD(target, upstreamModel, utf8.RuneCountInString(redactedInput))
	}
	entry.Status, entry.Cached = http.StatusOK, res.Cache == cacheHit
	s.finalizeAudioUsage(r.Context(), entry, ak.KeyID, costMicro, 0, ttsChars, res.Cache)

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
//
// Each target speaks the requested voice as its own options map it (see
// routing.TargetOptions.SpeakAs); a target that cannot place the voice is
// passed over as unable to speak it, as is one whose upstream refuses the
// name, so an unfamiliar voice moves the request on instead of failing it.
// A WAV request — the default — is only answered with a readable WAV: a
// tier that returns anything else is treated as failed, because the client
// reads the sample rate from the header. The result names the model that
// actually spoke, so the ledger prices it.
//
// On an alias with the synthesis cache, each attempt first looks for the
// clip its own target rendered of this text in the mapped voice, so the
// tier is chosen before the cache is asked and a clip is only ever served
// for the provider and voice that spoke it. A hit answers without calling
// the provider and is not counted as an attempt; a clip a provider renders
// is stored for the next request.
func (s *Server) runSynthesize(ctx context.Context, plan *routing.Plan, req audio.SpeechRequest) (audio.SpeechResponse, execResult, error) {
	var resp audio.SpeechResponse
	var served string
	var cache cacheOutcome
	var key speechcache.Key
	supports := func(p providers.Provider) error {
		if _, ok := p.(providers.Synthesizer); !ok {
			return &providers.Error{Status: http.StatusBadRequest, Retryable: false, Message: "provider " + p.Name() + " does not support speech synthesis"}
		}
		return nil
	}
	res, _, err := s.executePlan(ctx, plan, supports, func(ctx context.Context, p providers.Provider, t routing.Target, _ func() bool) error {
		choice, ok := t.Options.SpeakAs(req.Voice, req.Language)
		if !ok {
			return &providers.Error{Status: http.StatusBadRequest, Code: providers.ErrCodeVoiceNotSupported,
				Message: "target " + t.Provider + "/" + t.UpstreamModel + " has no voice for " + req.Voice}
		}
		in := req
		in.Model, in.Voice = t.UpstreamModel, choice.Voice
		if choice.Model != "" {
			in.Model = choice.Model
		}
		cache = ""
		if plan.SynthesisCache {
			key = speechcache.Key{Alias: plan.Alias, Provider: t.Provider, Model: in.Model, Voice: in.Voice,
				Language: in.Language, Format: in.ResponseFormat, Text: in.Input}
			clip, hit, err := s.speechCache.Get(ctx, key)
			switch {
			case hit:
				resp = audio.SpeechResponse{Audio: clip.Audio, ContentType: clip.ContentType}
				served, cache = clip.Model, cacheHit
				return nil
			case err != nil:
				if !errors.Is(err, speechcache.ErrUnavailable) {
					slog.Warn("synthesis cache lookup failed", "alias", plan.Alias, "provider", t.Provider, "err", err)
				}
				cache = cacheError
			default:
				cache = cacheMiss
			}
		}
		out, err := p.(providers.Synthesizer).Synthesize(ctx, in)
		if err != nil {
			return err
		}
		if in.ResponseFormat == "wav" {
			if _, ok := audio.WAVSampleRate(out.Audio); !ok {
				return &providers.Error{Status: http.StatusBadGateway, Retryable: true,
					Message: "upstream " + p.Name() + " answered a wav request with " + out.ContentType + " that is not a readable WAV"}
			}
			out.ContentType = "audio/wav"
		}
		resp, served = out, in.Model
		if out.Model != "" {
			served = out.Model
		}
		return nil
	})
	if err != nil {
		return audio.SpeechResponse{}, res, err
	}
	res.UpstreamModel, res.Cache = served, cache
	switch cache {
	case cacheHit:
		// executePlan counted the attempt that found the clip, but no
		// upstream call was made for it.
		res.Attempts--
	case cacheMiss:
		// Stored even if the client has gone: the clip is already paid for.
		clip := speechcache.Clip{Audio: resp.Audio, ContentType: resp.ContentType, Model: served}
		if err := s.speechCache.Put(context.WithoutCancel(ctx), key, clip, plan.SynthesisCacheTTL); err != nil && !errors.Is(err, speechcache.ErrUnavailable) {
			slog.Warn("synthesis cache store failed", "alias", plan.Alias, "provider", res.Provider, "err", err)
		}
	}
	if cache != "" {
		s.metrics.SynthesisCache(plan.Alias, res.Provider, string(cache))
	}
	return resp, res, nil
}
