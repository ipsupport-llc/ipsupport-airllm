package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/audio"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/dlp"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/ledger"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/llm"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/providers"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/routing"
)

// handleVideoSpeech implements POST /v1/video/speech: the request of
// /v1/audio/speech (model, input, voice, language), answered by a talking-face
// video of the speech, a fragmented MP4 streamed as the upstream makes it.
// Policy, limits, DLP, the ledger (priced by characters, like speech) and
// capture work as for /v1/audio/speech; there is no synthesis cache.
func (s *Server) handleVideoSpeech(w http.ResponseWriter, r *http.Request) {
	ak, _ := keyFromContext(r.Context())
	start := time.Now()
	var body struct {
		Model    string `json:"model"`
		Input    string `json:"input"`
		Voice    string `json:"voice"`
		Language string `json:"language"`
		// Placeholder false: the stream starts with the face (no
		// "initializing" frames), for a player that shows its own idle clips.
		Placeholder *bool `json:"placeholder"`
	}
	// Plain decoding (unknown fields ignored), as for /v1/audio/speech.
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
	f, ok := w.(http.Flusher)
	if !ok {
		writeProtocolError(w, r, http.StatusInternalServerError, "api_error", "streaming is not supported here")
		return
	}
	sink := &speechSink{w: w, flush: f.Flush, expose: plan.ExposeBackendHeaders}
	res, streamed, callErr := s.runVideoTo(r.Context(), plan, audio.SpeechRequest{
		Input: redactedInput, Voice: body.Voice, Language: audio.CanonicalLanguage(body.Language), ResponseFormat: "mp4",
		Placeholder: body.Placeholder,
	}, sink)
	target, upstreamModel := res.Provider, res.UpstreamModel
	entry := ledger.Entry{
		KeyID: ak.KeyID, UserID: ak.UserID, Alias: body.Model, ProviderName: target, UpstreamModel: upstreamModel,
		IngressProtocol: "openai", UpstreamProtocol: "openai", Tier: res.Tier, Attempts: res.Attempts,
		Session: res.Session, LatencyMS: time.Since(start).Milliseconds(),
	}
	if callErr != nil && !streamed {
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
	chars := utf8.RuneCountInString(redactedInput)
	costMicro := s.pricing.TTSCostMicroUSD(target, upstreamModel, chars)
	entry.Status = http.StatusOK
	if callErr != nil {
		// Video already went out; the ledger keeps why it broke off.
		entry.ErrorMsg = callErr.Error()
	}
	s.finalizeAudioUsage(r.Context(), entry, ak.KeyID, costMicro, 0, int64(chars), "")
	dlpRes := dlpResult{}
	if len(findings) > 0 {
		dlpRes = dlpResult{Findings: findings, MsgFindings: [][]dlp.Finding{findings}, HadIncident: true,
			AlreadyRedacted: redactedInput != body.Input}
	}
	s.enqueueCapture(ak, "openai", body.Model, target, upstreamModel, http.StatusOK, 0, 0, float64(costMicro)/1e6, dlpRes,
		[]llm.Message{{Role: "user", Content: redactedInput}}, "")
}

// runVideoTo executes the plan for a talking-face video, skipping targets
// whose provider cannot make one. The first bytes are held until the attempt
// commits, then the video goes to the sink as it arrives; a break after that
// ends the reply (started reports it), before it the next tier is tried.
// Voices map as for speech, and the language always goes to the upstream
// (a talking-face TTS takes it as its voice).
func (s *Server) runVideoTo(ctx context.Context, plan *routing.Plan, req audio.SpeechRequest, sink *speechSink) (execResult, bool, error) {
	var served, label string
	supports := func(p providers.Provider) error {
		if _, ok := p.(providers.VideoSpeaker); !ok {
			return &providers.Error{Status: http.StatusBadRequest, Retryable: false, Message: "provider " + p.Name() + " does not make speech video"}
		}
		return nil
	}
	res, started, err := s.executePlan(ctx, plan, supports, func(ctx context.Context, p providers.Provider, t routing.Target, commit func() bool) error {
		choice, ok := t.Options.SpeakAs(req.Voice, req.Language)
		if !ok {
			return &providers.Error{Status: http.StatusBadRequest, Code: providers.ErrCodeVoiceNotSupported,
				Message: "target " + t.Provider + "/" + t.UpstreamModel + " has no voice for " + req.Voice}
		}
		in := req
		in.Model, in.Voice = t.UpstreamModel, choice.Voice
		if in.SendLanguage = req.Language; in.SendLanguage == "" {
			in.SendLanguage = audio.VoiceLanguage(req.Voice)
		}
		if choice.Model != "" {
			in.Model = choice.Model
		}
		st, err := p.(providers.VideoSpeaker).SpeakVideo(ctx, in)
		if err != nil {
			return err
		}
		defer st.Body.Close()
		if st.ContentType == "" {
			st.ContentType = "video/mp4"
		}
		_, _, err = streamSpeech(st, in, p, t, sink, commit, false)
		served, label = in.Model, t.DisplayLabel
		return err
	})
	res.UpstreamModel, res.DisplayLabel = served, label
	return res, sink.begun && started, err
}
