package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/audio"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/llm"
)

// KindGoogleTTS is the provider kind for Google Cloud Text-to-Speech.
const KindGoogleTTS = "google-tts"

// GoogleTTS is Google Cloud Text-to-Speech, reached over its native v1 REST
// API, which has no OpenAI-shaped surface. It authenticates as the Google
// Speech kind does — a short-lived token from the ambient identity or a
// stored service account — and names the configured project as the quota
// project, so synthesis is billed there.
//
// It synthesizes and lists its voices, nothing else; chat on it fails as a
// configuration mistake.
type GoogleTTS struct {
	name    string
	baseURL string // API root, e.g. https://texttospeech.googleapis.com
	project string
	tokens  TokenSource
	hc      *http.Client

	// The voice catalogue changes when Google ships voices, not per call,
	// so it is fetched once in a while rather than on every capabilities
	// request.
	voicesMu      sync.Mutex
	voices        []audio.Voice
	voicesFetched time.Time
}

// googleTTSVoicesTTL is how long a fetched voice catalogue is reused, and
// googleTTSVoicesTimeout how long one fetch may take.
const (
	googleTTSVoicesTTL     = 6 * time.Hour
	googleTTSVoicesTimeout = 10 * time.Second
)

// NewGoogleTTS builds a Google Text-to-Speech provider addressed at baseURL —
// see googleTTSBaseURL, which assembles it from the configuration — billed
// to project, authenticating with tokens. The location is part of the host,
// so it is not needed here.
func NewGoogleTTS(name, baseURL, project string, tokens TokenSource) *GoogleTTS {
	return &GoogleTTS{
		name:    name,
		baseURL: strings.TrimRight(baseURL, "/"),
		project: project,
		tokens:  tokens,
		hc:      &http.Client{},
	}
}

func (p *GoogleTTS) Name() string     { return p.name }
func (p *GoogleTTS) Kind() string     { return KindGoogleTTS }
func (p *GoogleTTS) Protocol() string { return "openai" }

func (p *GoogleTTS) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, p.noChat()
}

func (p *GoogleTTS) ChatStream(context.Context, llm.ChatRequest, func(llm.StreamChunk) error) error {
	return p.noChat()
}

func (p *GoogleTTS) noChat() error {
	return &Error{Status: http.StatusBadRequest, Message: "provider " + p.name + " (" + KindGoogleTTS + ") does not support chat"}
}

// googleTTSModels are the voice families Google prices separately, for the
// alias editor's dropdown and as price-row model names: a voice's family is
// the model the ledger records (see googleVoiceFamily).
var googleTTSModels = []string{"chirp3-hd", "chirp-hd", "neural2", "polyglot", "standard", "studio", "wavenet"}

// ListModels returns the curated voice families.
func (p *GoogleTTS) ListModels(context.Context) ([]string, error) {
	return append([]string(nil), googleTTSModels...), nil
}

// googleAudioEncoding is how Google is asked for one response_format, and
// the content type the audio comes back as.
type googleAudioEncoding struct {
	encoding, contentType string
}

// googleAudioEncodings are the response formats Google can answer in.
// LINEAR16 arrives with a WAV header, so it is the gateway's WAV.
var googleAudioEncodings = map[string]googleAudioEncoding{
	"wav":  {"LINEAR16", "audio/wav"},
	"mp3":  {"MP3", "audio/mpeg"},
	"opus": {"OGG_OPUS", "audio/ogg"},
}

// Synthesize speaks one text with a synchronous synthesize call.
//
// Google wants the voice's language beside its name; Google names start
// with it, so it is read from there, and otherwise taken from the request
// in Google's locale form. The model the ledger records is the voice's
// family, which is what Google bills by, whatever the target's own model
// label says.
func (p *GoogleTTS) Synthesize(ctx context.Context, in audio.SpeechRequest) (audio.SpeechResponse, error) {
	format := in.ResponseFormat
	if format == "" {
		format = "wav"
	}
	enc, ok := googleAudioEncodings[format]
	if !ok {
		return audio.SpeechResponse{}, &Error{Status: http.StatusBadRequest,
			Message: "provider " + p.name + " (" + KindGoogleTTS + ") cannot answer in response_format " + format + "; use wav, mp3 or opus"}
	}
	lang := audio.VoiceLanguage(in.Voice)
	if lang == "" {
		lang = googleLocale(in.Language)
	}
	if lang == "" {
		lang = "en-US"
	}

	var body struct {
		Input struct {
			Text string `json:"text"`
		} `json:"input"`
		Voice struct {
			LanguageCode string `json:"languageCode"`
			Name         string `json:"name,omitempty"`
		} `json:"voice"`
		AudioConfig struct {
			AudioEncoding string `json:"audioEncoding"`
		} `json:"audioConfig"`
	}
	body.Input.Text = in.Input
	body.Voice.LanguageCode = lang
	body.Voice.Name = in.Voice
	body.AudioConfig.AudioEncoding = enc.encoding
	payload, err := json.Marshal(body)
	if err != nil {
		return audio.SpeechResponse{}, err
	}

	var out struct {
		AudioContent []byte `json:"audioContent"` // base64 on the wire
	}
	if err := p.do(ctx, http.MethodPost, "/v1/text:synthesize", payload, &out); err != nil {
		return audio.SpeechResponse{}, classifyVoiceRejection(err)
	}
	model := googleVoiceFamily(in.Voice)
	if model == "" {
		model = in.Model
	}
	return audio.SpeechResponse{Audio: out.AudioContent, ContentType: enc.contentType, Model: model}, nil
}

// googleVoiceFamily is the family a Google voice name carries between its
// language and its own letter or name — "en-US-Chirp3-HD-Charon" is
// chirp3-hd, "en-US-Neural2-F" neural2 — in lower case, as price rows spell
// it. Empty for a name that does not have that shape.
func googleVoiceFamily(voice string) string {
	if audio.VoiceLanguage(voice) == "" {
		return ""
	}
	parts := strings.SplitN(strings.ReplaceAll(voice, "_", "-"), "-", 3)
	rest := parts[2]
	i := strings.LastIndex(rest, "-")
	if i <= 0 {
		return ""
	}
	return strings.ToLower(rest[:i])
}

// ListVoices returns Google's voice catalogue: each voice's name, its first
// language and its gender.
func (p *GoogleTTS) ListVoices(ctx context.Context) ([]audio.Voice, error) {
	p.voicesMu.Lock()
	defer p.voicesMu.Unlock()
	if p.voices != nil && time.Since(p.voicesFetched) < googleTTSVoicesTTL {
		return append([]audio.Voice(nil), p.voices...), nil
	}
	var out struct {
		Voices []struct {
			LanguageCodes []string `json:"languageCodes"`
			Name          string   `json:"name"`
			SSMLGender    string   `json:"ssmlGender"`
		} `json:"voices"`
	}
	// The lock is held across the fetch so concurrent callers share one;
	// the timeout keeps a hanging upstream from holding them all.
	ctx, cancel := context.WithTimeout(ctx, googleTTSVoicesTimeout)
	defer cancel()
	if err := p.do(ctx, http.MethodGet, "/v1/voices", nil, &out); err != nil {
		return nil, err
	}
	voices := make([]audio.Voice, 0, len(out.Voices))
	for _, v := range out.Voices {
		lang := audio.VoiceLanguage(v.Name)
		if len(v.LanguageCodes) > 0 {
			lang = audio.CanonicalLanguage(v.LanguageCodes[0])
		}
		voices = append(voices, audio.Voice{ID: v.Name, Language: lang, Gender: googleGender(v.SSMLGender)})
	}
	p.voices, p.voicesFetched = voices, time.Now()
	return append([]audio.Voice(nil), voices...), nil
}

// googleGender spells Google's SSML gender the way the gateway reports
// genders; "unspecified" is unknown, so empty.
func googleGender(g string) string {
	switch g {
	case "MALE":
		return "male"
	case "FEMALE":
		return "female"
	case "NEUTRAL":
		return "neutral"
	}
	return ""
}

// do sends one authenticated request and decodes the JSON reply into out.
func (p *GoogleTTS) do(ctx context.Context, method, path string, payload []byte, out any) error {
	token, err := p.tokens.Token(ctx)
	if err != nil {
		// Retryable, as for Vertex: a failed exchange will very likely work
		// again, and meanwhile another tier can speak.
		return transportError(fmt.Errorf("upstream %s token source: %w", p.name, err))
	}
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.baseURL+path, body)
	if err != nil {
		return err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Goog-User-Project", p.project)

	resp, err := p.hc.Do(req)
	if err != nil {
		return transportError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return httpError(p.name, resp.StatusCode, b, resp.Header)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("upstream %s: decode %s response: %w", p.name, path, err)
	}
	return nil
}

// googleTTSBaseURL resolves the API root: an explicit address verbatim (a
// stub, a proxy), otherwise the global host or the location's own.
func googleTTSBaseURL(cfg googleCloudConfig, explicit string) string {
	if explicit != "" {
		return strings.TrimRight(explicit, "/")
	}
	if loc := cfg.location(); loc != vertexGlobalLocation {
		return "https://" + loc + "-texttospeech.googleapis.com"
	}
	return "https://texttospeech.googleapis.com"
}

// validateGoogleTTSConfig reports why a Google Text-to-Speech provider
// could never serve a request: without a project there is no quota project
// to bill.
func validateGoogleTTSConfig(cfg googleCloudConfig) error {
	if cfg.Project == "" {
		return errors.New("google-tts provider needs a cloud project")
	}
	return nil
}
