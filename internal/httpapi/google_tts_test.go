package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/audio"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/providers"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/routing"
)

// These are the seam tests for the Google Text-to-Speech provider kind and
// for what every synthesis tier returns: requests are driven through the
// executor and the real providers into in-process stand-ins for Google's
// synthesize endpoint and an OpenAI-compatible speech route, and the
// assertions are on what those upstreams received and what came back out.

// ttsCall is one request the Google stand-in received.
type ttsCall struct {
	path, auth, quotaProject string
	body                     struct {
		Input struct {
			Text string `json:"text"`
		} `json:"input"`
		Voice struct {
			LanguageCode string `json:"languageCode"`
			Name         string `json:"name"`
		} `json:"voice"`
		AudioConfig struct {
			AudioEncoding string `json:"audioEncoding"`
		} `json:"audioConfig"`
	}
}

// googleVoicesReply is the stand-in's answer to the voice catalogue.
const googleVoicesReply = `{"voices":[
	{"languageCodes":["en-US"],"name":"en-US-Chirp3-HD-Charon","ssmlGender":"MALE","naturalSampleRateHertz":24000},
	{"languageCodes":["es-ES"],"name":"es-ES-Neural2-A","ssmlGender":"FEMALE","naturalSampleRateHertz":24000}]}`

// fakeGoogleTTS records every synthesize request and answers it with
// reply(call) — a status and a body; the voice catalogue is googleVoicesReply.
func fakeGoogleTTS(t *testing.T, reply func(ttsCall) (int, string)) (*httptest.Server, func() []ttsCall) {
	t.Helper()
	var mu sync.Mutex
	var calls []ttsCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && r.URL.Path == "/v1/voices" {
			fmt.Fprint(w, googleVoicesReply)
			return
		}
		c := ttsCall{path: r.URL.Path, auth: r.Header.Get("Authorization"), quotaProject: r.Header.Get("X-Goog-User-Project")}
		_ = json.NewDecoder(r.Body).Decode(&c.body)
		mu.Lock()
		calls = append(calls, c)
		mu.Unlock()
		status, body := reply(c)
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []ttsCall {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(calls)
	}
}

// googleSynthesized is Google's reply carrying wav as its audio content.
func googleSynthesized(wav []byte) string {
	return fmt.Sprintf(`{"audioContent":%q}`, base64.StdEncoding.EncodeToString(wav))
}

func newGoogleTTS(name, url string) *providers.GoogleTTS {
	return providers.NewGoogleTTS(name, url, "acme", stubTokens{token: "ya29.stub"})
}

// speechCallOpenAI is what the OpenAI-compatible stand-in received.
type speechCallOpenAI struct {
	Model          string `json:"model"`
	Input          string `json:"input"`
	Voice          string `json:"voice"`
	ResponseFormat string `json:"response_format"`
}

// fakeOpenAISpeech answers /audio/speech with the given content type and
// bytes, recording what it was asked.
func fakeOpenAISpeech(t *testing.T, contentType string, out []byte) (*httptest.Server, func() []speechCallOpenAI) {
	t.Helper()
	var mu sync.Mutex
	var calls []speechCallOpenAI
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var c speechCallOpenAI
		_ = json.NewDecoder(r.Body).Decode(&c)
		mu.Lock()
		calls = append(calls, c)
		mu.Unlock()
		w.Header().Set("Content-Type", contentType)
		_, _ = w.Write(out)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []speechCallOpenAI {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(calls)
	}
}

func ttsPlan(tiers ...routing.Target) *routing.Plan {
	p := &routing.Plan{Alias: "voice-tts", Strategy: "round_robin"}
	for i, t := range tiers {
		t.Tier = i
		p.Tiers = append(p.Tiers, []routing.Target{t})
	}
	return p
}

func sampleRate(t *testing.T, wav []byte) int {
	t.Helper()
	rate, ok := audio.WAVSampleRate(wav)
	if !ok {
		t.Fatalf("reply is not a readable WAV: %q", wav[:min(len(wav), 16)])
	}
	return rate
}

func TestGoogleTTSSpeaksTheRequestedVoiceAsWAV(t *testing.T) {
	up, calls := fakeGoogleTTS(t, func(ttsCall) (int, string) { return http.StatusOK, googleSynthesized(pcmWAV(24000, 1)) })
	s := newRunChatTestServer(t, newGoogleTTS("gtts", up.URL))

	out, res, err := s.runSynthesize(context.Background(), ttsPlan(routing.Target{Provider: "gtts", UpstreamModel: "neural2"}),
		audio.SpeechRequest{Input: "Hello there", Voice: "en-US-Chirp3-HD-Charon", ResponseFormat: "wav"})
	if err != nil {
		t.Fatalf("runSynthesize: %v", err)
	}
	got := calls()
	if len(got) != 1 {
		t.Fatalf("upstream called %d times, want 1", len(got))
	}
	c := got[0]
	if c.path != "/v1/text:synthesize" {
		t.Errorf("path = %q, want /v1/text:synthesize", c.path)
	}
	if c.auth != "Bearer ya29.stub" || c.quotaProject != "acme" {
		t.Errorf("auth = %q, quota project = %q; want the minted token billed to the configured project", c.auth, c.quotaProject)
	}
	if c.body.Input.Text != "Hello there" || c.body.Voice.Name != "en-US-Chirp3-HD-Charon" || c.body.Voice.LanguageCode != "en-US" {
		t.Errorf("sent text %q voice %q language %q, want the voice and the language its name carries",
			c.body.Input.Text, c.body.Voice.Name, c.body.Voice.LanguageCode)
	}
	if c.body.AudioConfig.AudioEncoding != "LINEAR16" {
		t.Errorf("audioEncoding = %q, want LINEAR16 — Google's WAV", c.body.AudioConfig.AudioEncoding)
	}
	if out.ContentType != "audio/wav" || sampleRate(t, out.Audio) != 24000 {
		t.Errorf("content type %q, want audio/wav at Google's 24 kHz", out.ContentType)
	}
	if res.UpstreamModel != "chirp3-hd" {
		t.Errorf("served model = %q, want chirp3-hd — the voice family Google bills, not the target's label", res.UpstreamModel)
	}
}

func TestSynthesisFailsOverFromGoogleToTheMappedVoiceOnAnOpenAITier(t *testing.T) {
	google, _ := fakeGoogleTTS(t, func(ttsCall) (int, string) {
		return http.StatusServiceUnavailable, `{"error":{"code":503,"message":"unavailable","status":"UNAVAILABLE"}}`
	})
	openai, calls := fakeOpenAISpeech(t, "audio/wav", pcmWAV(24000, 1))
	s := newRunChatTestServer(t, newGoogleTTS("gtts", google.URL), providers.NewOpenAICompat("oai", "openai", openai.URL, ""))
	plan := ttsPlan(
		routing.Target{Provider: "gtts", UpstreamModel: "chirp3-hd"},
		routing.Target{Provider: "oai", UpstreamModel: "tts-1", Options: routing.TargetOptions{
			Voices: map[string]routing.VoiceChoice{"en-US-Chirp3-HD-Charon": {Model: "gpt-4o-mini-tts", Voice: "onyx"}},
		}},
	)

	out, res, err := s.runSynthesize(context.Background(), plan, audio.SpeechRequest{Input: "Hi", Voice: "en-US-Chirp3-HD-Charon", ResponseFormat: "wav"})
	if err != nil {
		t.Fatalf("runSynthesize: %v", err)
	}
	if res.Provider != "oai" || res.Tier != 1 {
		t.Fatalf("served by %q tier %d, want the OpenAI tier", res.Provider, res.Tier)
	}
	if c := calls()[0]; c.Model != "gpt-4o-mini-tts" || c.Voice != "onyx" || c.ResponseFormat != "wav" {
		t.Errorf("OpenAI got model %q voice %q format %q, want the mapped gpt-4o-mini-tts/onyx as wav", c.Model, c.Voice, c.ResponseFormat)
	}
	if res.UpstreamModel != "gpt-4o-mini-tts" {
		t.Errorf("served model = %q, want the mapped one — the ledger prices what ran", res.UpstreamModel)
	}
	if sampleRate(t, out.Audio) != 24000 {
		t.Error("want the OpenAI tier's WAV passed through")
	}
}

func TestAnUnmappedVoiceSpeaksWithTheLanguageDefault(t *testing.T) {
	openai, calls := fakeOpenAISpeech(t, "audio/wav", pcmWAV(22050, 1))
	s := newRunChatTestServer(t, providers.NewOpenAICompat("piper", "openai", openai.URL, ""))
	plan := ttsPlan(routing.Target{Provider: "piper", UpstreamModel: "piper", Options: routing.TargetOptions{
		Voices:        map[string]routing.VoiceChoice{"en-US-Chirp3-HD-Charon": {Voice: "en_US-ryan-high"}},
		DefaultVoices: map[string]routing.VoiceChoice{"es": {Voice: "es_ES-davefx-medium"}, "en": {Voice: "en_US-lessac-medium"}},
	}})

	for voice, want := range map[string]string{
		"es-ES-Neural2-A":        "es_ES-davefx-medium", // the language its name carries
		"en-GB-Neural2-B":        "en_US-lessac-medium", // a bare-language default covers every region
		"en-US-Chirp3-HD-Charon": "en_US-ryan-high",     // a mapped voice wins over the default
	} {
		out, res, err := s.runSynthesize(context.Background(), plan, audio.SpeechRequest{Input: "Hola", Voice: voice, ResponseFormat: "wav"})
		if err != nil {
			t.Fatalf("%s: %v", voice, err)
		}
		got := calls()
		if c := got[len(got)-1]; c.Voice != want || c.Model != "piper" {
			t.Errorf("%s: sent voice %q model %q, want %q with the target's model", voice, c.Voice, c.Model, want)
		}
		if res.UpstreamModel != "piper" || sampleRate(t, out.Audio) != 22050 {
			t.Errorf("%s: served model %q, want piper at Piper's own 22.05 kHz", voice, res.UpstreamModel)
		}
	}

	// A request may name the language itself, for a voice whose name does not.
	if _, _, err := s.runSynthesize(context.Background(), plan, audio.SpeechRequest{Input: "Hola", Voice: "alloy", Language: "es-MX", ResponseFormat: "wav"}); err != nil {
		t.Fatalf("named language: %v", err)
	}
	if got := calls(); got[len(got)-1].Voice != "es_ES-davefx-medium" {
		t.Errorf("voice with the language in the request: sent %q, want the Spanish default", got[len(got)-1].Voice)
	}
}

func TestAnUnknownVoiceNeverAbortsTheTierChain(t *testing.T) {
	// Tier 0 has a voice map but nothing for German; tier 1 is Google,
	// which refuses the voice outright; tier 2 maps it.
	piper, piperCalls := fakeOpenAISpeech(t, "audio/wav", pcmWAV(22050, 1))
	google, _ := fakeGoogleTTS(t, func(ttsCall) (int, string) {
		return http.StatusBadRequest, `{"error":{"code":400,"message":"Voice 'de-DE-Fancy-X' does not exist. Is it misspelled?","status":"INVALID_ARGUMENT"}}`
	})
	openai, openaiCalls := fakeOpenAISpeech(t, "audio/wav", pcmWAV(24000, 1))
	s := newRunChatTestServer(t,
		providers.NewOpenAICompat("piper", "openai", piper.URL, ""),
		newGoogleTTS("gtts", google.URL),
		providers.NewOpenAICompat("oai", "openai", openai.URL, ""))
	plan := ttsPlan(
		routing.Target{Provider: "piper", UpstreamModel: "piper", Options: routing.TargetOptions{
			DefaultVoices: map[string]routing.VoiceChoice{"en": {Voice: "en_US-lessac-medium"}},
		}},
		routing.Target{Provider: "gtts", UpstreamModel: "neural2"},
		routing.Target{Provider: "oai", UpstreamModel: "tts-1", Options: routing.TargetOptions{
			DefaultVoices: map[string]routing.VoiceChoice{"de": {Voice: "echo"}},
		}},
	)

	_, res, err := s.runSynthesize(context.Background(), plan, audio.SpeechRequest{Input: "Hallo", Voice: "de-DE-Fancy-X", ResponseFormat: "wav"})
	if err != nil {
		t.Fatalf("runSynthesize: %v — an unknown voice must move on, not fail the request", err)
	}
	if res.Provider != "oai" || res.Tier != 2 {
		t.Errorf("served by %q tier %d, want the tier that maps German", res.Provider, res.Tier)
	}
	if n := len(piperCalls()); n != 0 {
		t.Errorf("the tier that cannot map the voice was called %d times, want it skipped", n)
	}
	if c := openaiCalls()[0]; c.Voice != "echo" {
		t.Errorf("OpenAI got voice %q, want the German default", c.Voice)
	}
}

func TestATierThatAnswersWithoutWAVFallsThrough(t *testing.T) {
	mp3, _ := fakeOpenAISpeech(t, "audio/mpeg", []byte("ID3\x03\x00not a wav at all"))
	wav, _ := fakeOpenAISpeech(t, "audio/wav", pcmWAV(16000, 1))
	s := newRunChatTestServer(t, providers.NewOpenAICompat("mp3", "openai", mp3.URL, ""), providers.NewOpenAICompat("wav", "openai", wav.URL, ""))
	plan := ttsPlan(routing.Target{Provider: "mp3", UpstreamModel: "m"}, routing.Target{Provider: "wav", UpstreamModel: "m"})

	out, res, err := s.runSynthesize(context.Background(), plan, audio.SpeechRequest{Input: "Hi", Voice: "alloy", ResponseFormat: "wav"})
	if err != nil {
		t.Fatalf("runSynthesize: %v", err)
	}
	if res.Provider != "wav" || out.ContentType != "audio/wav" || sampleRate(t, out.Audio) != 16000 {
		t.Errorf("served by %q as %q, want the tier that really returned WAV", res.Provider, out.ContentType)
	}
}

func TestGoogleTTSListsItsVoices(t *testing.T) {
	up, _ := fakeGoogleTTS(t, func(ttsCall) (int, string) { return http.StatusOK, "{}" })
	voices, err := newGoogleTTS("gtts", up.URL).ListVoices(context.Background())
	if err != nil {
		t.Fatalf("ListVoices: %v", err)
	}
	want := []audio.Voice{
		{ID: "en-US-Chirp3-HD-Charon", Language: "en-US", Gender: "male"},
		{ID: "es-ES-Neural2-A", Language: "es-ES", Gender: "female"},
	}
	if !slices.Equal(voices, want) {
		t.Errorf("voices = %+v, want %+v", voices, want)
	}
}

func TestAVoiceNoTierCanSpeakIsTheClientsMistake(t *testing.T) {
	openai, calls := fakeOpenAISpeech(t, "audio/wav", pcmWAV(24000, 1))
	s := newRunChatTestServer(t, providers.NewOpenAICompat("oai", "openai", openai.URL, ""))
	plan := ttsPlan(routing.Target{Provider: "oai", UpstreamModel: "tts-1", Options: routing.TargetOptions{
		DefaultVoices: map[string]routing.VoiceChoice{"en": {Voice: "onyx"}},
	}})

	_, _, err := s.runSynthesize(context.Background(), plan, audio.SpeechRequest{Input: "Hallo", Voice: "de-DE-Neural2-B", ResponseFormat: "wav"})
	if err == nil {
		t.Fatal("a voice no tier can speak was spoken")
	}
	if code, _ := classifyUpstreamErr(err); code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 — nothing behind the alias speaks that voice, the upstreams did not fail", code)
	}
	if n := len(calls()); n != 0 {
		t.Errorf("upstream called %d times, want none", n)
	}
}

func TestARequestWithoutAVoiceLeavesItToTheProvider(t *testing.T) {
	openai, calls := fakeOpenAISpeech(t, "audio/wav", pcmWAV(24000, 1))
	s := newRunChatTestServer(t, providers.NewOpenAICompat("oai", "openai", openai.URL, ""))
	plan := ttsPlan(routing.Target{Provider: "oai", UpstreamModel: "tts-1", Options: routing.TargetOptions{
		Voices: map[string]routing.VoiceChoice{"en-US-Neural2-F": {Voice: "nova"}},
	}})

	if _, _, err := s.runSynthesize(context.Background(), plan, audio.SpeechRequest{Input: "Hi", ResponseFormat: "wav"}); err != nil {
		t.Fatalf("runSynthesize: %v — a voice map must not refuse a request that names no voice", err)
	}
	if c := calls()[0]; c.Voice != "" {
		t.Errorf("sent voice %q, want none", c.Voice)
	}
}
