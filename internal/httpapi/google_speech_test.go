package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/audio"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/providers"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/routing"
)

// These are the seam tests for the Google Speech provider kind and for the
// transcription result every tier returns: requests are driven through the
// executor and the real providers into in-process stand-ins for Google's
// recognize endpoint and an OpenAI-compatible transcription route, and the
// assertions are on what those upstreams received and what came back out.

// speechCall is one request the Google stand-in received.
type speechCall struct {
	path, auth, quotaProject string
	body                     struct {
		Config struct {
			AutoDecodingConfig *struct{} `json:"autoDecodingConfig"`
			LanguageCodes      []string  `json:"languageCodes"`
			Model              string    `json:"model"`
			Features           struct {
				EnableAutomaticPunctuation bool `json:"enableAutomaticPunctuation"`
			} `json:"features"`
		} `json:"config"`
		Content string `json:"content"`
	}
}

// fakeGoogleSpeech records every recognize request and answers it with
// reply(call) — a status and a body.
func fakeGoogleSpeech(t *testing.T, reply func(speechCall) (int, string)) (*httptest.Server, func() []speechCall) {
	t.Helper()
	var mu sync.Mutex
	var calls []speechCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := speechCall{path: r.URL.Path, auth: r.Header.Get("Authorization"), quotaProject: r.Header.Get("X-Goog-User-Project")}
		_ = json.NewDecoder(r.Body).Decode(&c.body)
		mu.Lock()
		calls = append(calls, c)
		mu.Unlock()
		status, body := reply(c)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []speechCall {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(calls)
	}
}

func newGoogleSpeech(name, url string) *providers.GoogleSpeech {
	return providers.NewGoogleSpeech(name, url, "acme", "global", stubTokens{token: "ya29.stub"})
}

// googleRecognized is Google's reply shape: one result per stretch of
// speech, each with its own language, and the billed duration in metadata.
const googleRecognized = `{
	"results":[
		{"alternatives":[{"transcript":"hello there","confidence":0.9}],"resultEndOffset":"1.200s","languageCode":"en-us"},
		{"alternatives":[{"transcript":" how are you ","confidence":0.7}],"resultEndOffset":"2.900s","languageCode":"en-us"}
	],
	"metadata":{"totalBilledDuration":"3s"}}`

func googleSpeechPlan(t routing.Target) *routing.Plan {
	return &routing.Plan{Alias: "voice-stt", Strategy: "round_robin", Tiers: [][]routing.Target{{t}}}
}

func TestGoogleSpeechRecognisesWithTheModelForTheLanguage(t *testing.T) {
	up, calls := fakeGoogleSpeech(t, func(speechCall) (int, string) { return http.StatusOK, googleRecognized })
	s := newRunChatTestServer(t, newGoogleSpeech("gstt", up.URL))
	plan := googleSpeechPlan(routing.Target{Provider: "gstt", UpstreamModel: "long", Options: routing.TargetOptions{
		RecognitionModels: map[string]string{"en-US": "telephony", "es": "telephony"},
	}})

	wav := []byte("RIFF....WAVEfmt fake")
	tr, res, err := s.runTranscribe(context.Background(), plan, audio.TranscriptionRequest{
		Audio: wav, Filename: "u.wav", Language: "en-US",
		AlternativeLanguages: []string{"es", "en-US", "de-DE", "fr", "it"},
	})
	if err != nil {
		t.Fatalf("runTranscribe: %v", err)
	}

	got := calls()
	if len(got) != 1 {
		t.Fatalf("upstream called %d times, want 1", len(got))
	}
	c := got[0]
	if c.path != "/v2/projects/acme/locations/global/recognizers/_:recognize" {
		t.Errorf("path = %q, want the project's default recognizer", c.path)
	}
	if c.auth != "Bearer ya29.stub" || c.quotaProject != "acme" {
		t.Errorf("auth = %q, quota project = %q; want the minted token billed to the configured project", c.auth, c.quotaProject)
	}
	if c.body.Config.Model != "telephony" {
		t.Errorf("model = %q, want telephony — the tier maps en-US to it", c.body.Config.Model)
	}
	if want := []string{"en-US", "es-ES", "de-DE", "fr-FR"}; !slices.Equal(c.body.Config.LanguageCodes, want) {
		t.Errorf("languageCodes = %v, want %v: primary first, regions filled in, duplicates dropped, four at most", c.body.Config.LanguageCodes, want)
	}
	if c.body.Config.AutoDecodingConfig == nil {
		t.Error("autoDecodingConfig missing — Google must read the format from the WAV header")
	}
	if !c.body.Config.Features.EnableAutomaticPunctuation {
		t.Error("automatic punctuation not requested")
	}
	if dec, _ := base64.StdEncoding.DecodeString(c.body.Content); string(dec) != string(wav) {
		t.Errorf("content = %q, want the uploaded audio", dec)
	}

	if tr.Text != "hello there how are you" {
		t.Errorf("text = %q, want the results joined", tr.Text)
	}
	if tr.Language != "en-US" {
		t.Errorf("language = %q, want en-US in canonical case", tr.Language)
	}
	if math.Abs(tr.Confidence-0.8) > 1e-9 {
		t.Errorf("confidence = %v, want 0.8 — the mean of the results'", tr.Confidence)
	}
	if tr.DurationSeconds != 3 {
		t.Errorf("duration = %v, want the billed 3s", tr.DurationSeconds)
	}
	if res.UpstreamModel != "telephony" {
		t.Errorf("served model = %q, want telephony — the ledger prices what actually ran", res.UpstreamModel)
	}
}

func TestGoogleSpeechUsesTheTargetModelForOtherLanguages(t *testing.T) {
	up, calls := fakeGoogleSpeech(t, func(speechCall) (int, string) {
		return http.StatusOK, `{"results":[{"alternatives":[{"transcript":"привет"}],"languageCode":"ru-ru"}],"metadata":{"totalBilledDuration":"1.500s"}}`
	})
	s := newRunChatTestServer(t, newGoogleSpeech("gstt", up.URL))
	plan := googleSpeechPlan(routing.Target{Provider: "gstt", UpstreamModel: "long", Options: routing.TargetOptions{
		RecognitionModels: map[string]string{"en-US": "telephony"},
	}})

	tr, res, err := s.runTranscribe(context.Background(), plan, audio.TranscriptionRequest{Audio: []byte("wav"), Language: "ru"})
	if err != nil {
		t.Fatalf("runTranscribe: %v", err)
	}
	c := calls()[0]
	if c.body.Config.Model != "long" || !slices.Equal(c.body.Config.LanguageCodes, []string{"ru-RU"}) {
		t.Errorf("sent model %q languages %v, want long for [ru-RU]", c.body.Config.Model, c.body.Config.LanguageCodes)
	}
	if res.UpstreamModel != "long" || tr.Language != "ru-RU" || tr.DurationSeconds != 1.5 || tr.Confidence != 0 {
		t.Errorf("model=%q language=%q duration=%v confidence=%v, want long ru-RU 1.5 and no confidence reported",
			res.UpstreamModel, tr.Language, tr.DurationSeconds, tr.Confidence)
	}
}

// whisperCall is what the OpenAI-compatible stand-in received.
type whisperCall struct {
	model, language, format string
	alternatives            []string
}

func fakeWhisper(t *testing.T, reply string) (*httptest.Server, *whisperCall) {
	t.Helper()
	got := &whisperCall{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("whisper stand-in: %v", err)
		}
		got.model, got.language, got.format = r.FormValue("model"), r.FormValue("language"), r.FormValue("response_format")
		got.alternatives = append(r.MultipartForm.Value["alternative_languages"], r.MultipartForm.Value["alternative_languages[]"]...)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, reply)
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

func TestTranscriptionFailsOverFromGoogleToAWhisperTierWithTheSameRequest(t *testing.T) {
	google, _ := fakeGoogleSpeech(t, func(speechCall) (int, string) {
		return http.StatusForbidden, `{"error":{"code":403,"message":"Permission denied","status":"PERMISSION_DENIED"}}`
	})
	whisper, got := fakeWhisper(t, `{"text":"hi there","language":"english","duration":2.5,
		"segments":[{"avg_logprob":-0.5},{"avg_logprob":-1.5}]}`)
	s := newRunChatTestServer(t, newGoogleSpeech("gstt", google.URL), providers.NewOpenAICompat("whisper", "openai", whisper.URL, ""))
	plan := &routing.Plan{Alias: "voice-stt", Strategy: "round_robin", Tiers: [][]routing.Target{
		{{Provider: "gstt", UpstreamModel: "long", Options: routing.TargetOptions{FallbackOnAuth: boolp(true)}}},
		{{Provider: "whisper", UpstreamModel: "large-v3-turbo", Tier: 1}},
	}}

	tr, res, err := s.runTranscribe(context.Background(), plan, audio.TranscriptionRequest{
		Audio: []byte("wav"), Filename: "u.wav", Language: "en-US", AlternativeLanguages: []string{"es-ES"},
	})
	if err != nil {
		t.Fatalf("runTranscribe: %v", err)
	}
	if res.Provider != "whisper" || res.Tier != 1 || res.Attempts != 2 {
		t.Fatalf("served by %q tier %d after %d attempts, want the whisper tier after the refused Google one", res.Provider, res.Tier, res.Attempts)
	}
	if got.language != "en" {
		t.Errorf("whisper got language %q, want en — the Whisper family rejects a region", got.language)
	}
	if len(got.alternatives) != 0 {
		t.Errorf("whisper got alternatives %v, want none — it cannot use them", got.alternatives)
	}
	if got.format != "verbose_json" {
		t.Errorf("whisper got response_format %q, want verbose_json", got.format)
	}
	if tr.Text != "hi there" || tr.Language != "en" || tr.DurationSeconds != 2.5 {
		t.Errorf("text=%q language=%q duration=%v, want the whisper answer with the language as a code", tr.Text, tr.Language, tr.DurationSeconds)
	}
	if math.Abs(tr.Confidence-0.8) > 1e-9 {
		t.Errorf("confidence = %v, want 0.8 from the segments' mean log-probability", tr.Confidence)
	}
	if res.UpstreamModel != "large-v3-turbo" {
		t.Errorf("served model = %q, want the target's", res.UpstreamModel)
	}
}

func TestATierSilentOnLanguageReportsTheRequestedOne(t *testing.T) {
	whisper, _ := fakeWhisper(t, `{"text":"hola","duration":1}`)
	s := newRunChatTestServer(t, providers.NewOpenAICompat("whisper", "groq", whisper.URL, ""))
	plan := googleSpeechPlan(routing.Target{Provider: "whisper", UpstreamModel: "whisper-large-v3"})

	tr, _, err := s.runTranscribe(context.Background(), plan, audio.TranscriptionRequest{Audio: []byte("wav"), Language: "es-ES"})
	if err != nil {
		t.Fatalf("runTranscribe: %v", err)
	}
	if tr.Language != "es-ES" || tr.Confidence != 0 {
		t.Errorf("language=%q confidence=%v, want the requested es-ES and no confidence", tr.Language, tr.Confidence)
	}
}

func TestGoogleSpeechDropsPunctuationARecognizerRejects(t *testing.T) {
	up, calls := fakeGoogleSpeech(t, func(c speechCall) (int, string) {
		if c.body.Config.Features.EnableAutomaticPunctuation {
			return http.StatusBadRequest, `{"error":{"code":400,"message":"Recognizer does not support feature: automatic_punctuation","status":"INVALID_ARGUMENT"}}`
		}
		return http.StatusOK, `{"results":[{"alternatives":[{"transcript":"ok"}],"languageCode":"el-gr"}]}`
	})
	s := newRunChatTestServer(t, newGoogleSpeech("gstt", up.URL))
	plan := googleSpeechPlan(routing.Target{Provider: "gstt", UpstreamModel: "chirp_3"})

	for i := range 2 {
		tr, _, err := s.runTranscribe(context.Background(), plan, audio.TranscriptionRequest{Audio: []byte("wav"), Language: "el-GR"})
		if err != nil || tr.Text != "ok" {
			t.Fatalf("transcription %d: text=%q err=%v, want it recognised without punctuation", i+1, tr.Text, err)
		}
	}
	if n := len(calls()); n != 3 {
		t.Errorf("upstream called %d times, want 3 — one rejected attempt, then the feature stays off for that model and language", n)
	}
}
