package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/audio"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/llm"
)

// KindGoogleSpeech is the provider kind for Google Cloud Speech-to-Text.
const KindGoogleSpeech = "google-speech"

// GoogleSpeech is Google Cloud Speech-to-Text, reached over its native v2
// REST API: unlike Vertex, Speech has no OpenAI-shaped surface to borrow, so
// this adapter owns the translation both ways. It authenticates the way the
// Vertex kind does — a short-lived token from the ambient identity or a
// stored service account, consulted per request — and names the configured
// project as the quota project, so recognition is billed there whatever
// project the identity itself belongs to.
//
// It transcribes and nothing else. Chat on it is a configuration mistake and
// fails as one rather than pretending to be a provider that cannot answer.
type GoogleSpeech struct {
	name     string
	baseURL  string // API root, e.g. https://speech.googleapis.com
	project  string
	location string
	tokens   TokenSource
	hc       *http.Client

	// noPunctuation remembers the model and language pairs whose recognizer
	// refused automatic punctuation, so each refusal is paid for once and
	// not on every utterance.
	noPunctuation sync.Map
}

// NewGoogleSpeech builds a Google Speech provider addressed at baseURL —
// see googleSpeechBaseURL, which assembles it from the configuration —
// recognising in project and location, authenticating with tokens.
func NewGoogleSpeech(name, baseURL, project, location string, tokens TokenSource) *GoogleSpeech {
	if location == "" {
		location = vertexGlobalLocation
	}
	return &GoogleSpeech{
		name:     name,
		baseURL:  strings.TrimRight(baseURL, "/"),
		project:  project,
		location: location,
		tokens:   tokens,
		hc:       &http.Client{},
	}
}

func (p *GoogleSpeech) Name() string     { return p.name }
func (p *GoogleSpeech) Kind() string     { return KindGoogleSpeech }
func (p *GoogleSpeech) Protocol() string { return "openai" }

func (p *GoogleSpeech) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, p.noChat()
}

func (p *GoogleSpeech) ChatStream(context.Context, llm.ChatRequest, func(llm.StreamChunk) error) error {
	return p.noChat()
}

func (p *GoogleSpeech) noChat() error {
	return &Error{Status: http.StatusBadRequest, Message: "provider " + p.name + " (" + KindGoogleSpeech + ") does not support chat"}
}

// googleSpeechModels is a hand-maintained list for the alias editor's
// dropdown; Speech publishes no model catalogue. A model missing here can
// still be typed in by hand.
var googleSpeechModels = []string{"chirp_3", "long", "short", "telephony", "telephony_short"}

// ListModels returns the curated recognition models.
func (p *GoogleSpeech) ListModels(context.Context) ([]string, error) {
	return append([]string(nil), googleSpeechModels...), nil
}

// RecognitionLanguages is the telephony-grade subset this gateway offers
// for Google recognition — Google supports far more, but the list mirrors
// the Whisper one so a pipeline's choices stay the same whichever recogniser
// leads an alias.
func (p *GoogleSpeech) RecognitionLanguages() []string {
	return append([]string(nil), audio.TelephonyLanguages...)
}

// googleSpeechMaxLanguages is Google's cap on languageCodes per request.
const googleSpeechMaxLanguages = 4

// Transcribe recognises one utterance with a synchronous recognize call.
//
// The language is Google's locale form, so a bare language gets its usual
// region ("uk" → "uk-UA"); alternatives follow the primary in the same list.
// The model is the target's per-language choice when it has one for the
// primary language — looked up by Google's locale, then by the language as
// the client spelled it, since "zh" becomes "cmn-Hans-CN" — and the target's
// model otherwise.
//
// Punctuation is requested because it makes transcripts readable, but some
// recognizers refuse it for some languages and fail the whole request. It is
// cosmetic, so a refusal drops it and retries, and the pair is remembered.
func (p *GoogleSpeech) Transcribe(ctx context.Context, in audio.TranscriptionRequest) (audio.TranscriptionResponse, error) {
	lang := googleLocale(in.Language)
	if lang == "" {
		lang = "en-US"
	}
	codes := []string{lang}
	for _, alt := range in.AlternativeLanguages {
		if len(codes) == googleSpeechMaxLanguages {
			break
		}
		if alt = googleLocale(alt); alt != "" && !strings.EqualFold(alt, lang) && !containsFold(codes, alt) {
			codes = append(codes, alt)
		}
	}
	model := in.Model
	if m, ok := audio.ForLanguage(in.ModelByLanguage, lang); ok {
		model = m
	} else if m, ok := audio.ForLanguage(in.ModelByLanguage, in.Language); ok {
		model = m
	}

	pair := model + "\x00" + lang
	_, refused := p.noPunctuation.Load(pair)
	resp, err := p.recognize(ctx, model, codes, in.Audio, !refused)
	if err != nil && !refused && refusesPunctuation(err) {
		p.noPunctuation.Store(pair, struct{}{})
		resp, err = p.recognize(ctx, model, codes, in.Audio, false)
	}
	if err != nil {
		return audio.TranscriptionResponse{}, err
	}
	out := resp.transcription()
	out.Model = model
	// Google names the billed duration in metadata. Without it the
	// utterance would be free and invisible to audio-second caps, so it is
	// metered by the WAV's own length, rounded up to the second as Google
	// bills.
	if out.DurationSeconds == 0 {
		if d, ok := audio.WAVDuration(in.Audio); ok {
			out.DurationSeconds = math.Ceil(d)
		}
	}
	return out, nil
}

func containsFold(list []string, s string) bool {
	for _, v := range list {
		if strings.EqualFold(v, s) {
			return true
		}
	}
	return false
}

// refusesPunctuation recognises Google refusing the punctuation feature.
// The message is the only signal: it arrives as a plain INVALID_ARGUMENT.
func refusesPunctuation(err error) bool {
	var pe *Error
	return errors.As(err, &pe) && pe.Status == http.StatusBadRequest &&
		strings.Contains(strings.ToLower(pe.Message), "punctuation")
}

type googleRecognizeRequest struct {
	Config struct {
		AutoDecodingConfig struct{} `json:"autoDecodingConfig"`
		LanguageCodes      []string `json:"languageCodes"`
		Model              string   `json:"model"`
		Features           struct {
			EnableAutomaticPunctuation bool `json:"enableAutomaticPunctuation,omitempty"`
		} `json:"features"`
	} `json:"config"`
	Content []byte `json:"content"` // base64 on the wire, as Google expects
}

type googleRecognizeResponse struct {
	Results []struct {
		Alternatives []struct {
			Transcript string  `json:"transcript"`
			Confidence float64 `json:"confidence"`
		} `json:"alternatives"`
		LanguageCode string `json:"languageCode"`
	} `json:"results"`
	Metadata struct {
		TotalBilledDuration string `json:"totalBilledDuration"`
	} `json:"metadata"`
}

// transcription folds Google's results into one transcript: the top
// alternative of each, joined; the mean of the confidences Google reported
// (it leaves some at zero, which means "not computed", not "certainly
// wrong"); the language of the last result that named one; and the billed
// duration, which is what the gateway prices.
func (r googleRecognizeResponse) transcription() audio.TranscriptionResponse {
	var out audio.TranscriptionResponse
	var parts []string
	var confSum float64
	var confN int
	for _, res := range r.Results {
		if len(res.Alternatives) == 0 {
			continue
		}
		top := res.Alternatives[0]
		if t := strings.TrimSpace(top.Transcript); t != "" {
			parts = append(parts, t)
		}
		if top.Confidence > 0 {
			confSum += top.Confidence
			confN++
		}
		if res.LanguageCode != "" {
			out.Language = audio.CanonicalLanguage(res.LanguageCode)
		}
	}
	out.Text = strings.Join(parts, " ")
	if confN > 0 {
		out.Confidence = confSum / float64(confN)
	}
	out.DurationSeconds = parseGoogleDuration(r.Metadata.TotalBilledDuration)
	return out
}

// parseGoogleDuration reads a protobuf Duration in its JSON form ("3s",
// "1.500s"). Anything else is zero: an unpriced utterance beats a failed one.
func parseGoogleDuration(s string) float64 {
	v, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(s), "s"), 64)
	if err != nil || v < 0 {
		return 0
	}
	return v
}

func (p *GoogleSpeech) recognize(ctx context.Context, model string, langs []string, wav []byte, punctuation bool) (googleRecognizeResponse, error) {
	var body googleRecognizeRequest
	body.Config.LanguageCodes = langs
	body.Config.Model = model
	body.Config.Features.EnableAutomaticPunctuation = punctuation
	body.Content = wav
	payload, err := json.Marshal(body)
	if err != nil {
		return googleRecognizeResponse{}, err
	}

	token, err := p.tokens.Token(ctx)
	if err != nil {
		// Retryable, as for Vertex: a failed exchange will very likely work
		// again, and meanwhile another tier can serve this utterance.
		return googleRecognizeResponse{}, transportError(fmt.Errorf("upstream %s token source: %w", p.name, err))
	}
	endpoint := fmt.Sprintf("%s/v2/projects/%s/locations/%s/recognizers/_:recognize",
		p.baseURL, url.PathEscape(p.project), url.PathEscape(p.location))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return googleRecognizeResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Goog-User-Project", p.project)

	resp, err := p.hc.Do(req)
	if err != nil {
		return googleRecognizeResponse{}, transportError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return googleRecognizeResponse{}, httpError(p.name, resp.StatusCode, b, resp.Header)
	}
	var out googleRecognizeResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return googleRecognizeResponse{}, fmt.Errorf("upstream %s: decode recognize response: %w", p.name, err)
	}
	return out, nil
}

// googleLocale turns a BCP-47 tag into the locale Google recognises: a tag
// with a region keeps it, a bare language gets its usual one, and a language
// without a usual region is passed on for Google to judge.
func googleLocale(tag string) string {
	tag = audio.CanonicalLanguage(tag)
	if tag == "" || strings.Contains(tag, "-") {
		return tag
	}
	if loc, ok := googleDefaultLocales[tag]; ok {
		return loc
	}
	return tag
}

// googleDefaultLocales gives each offered language (audio.TelephonyLanguages)
// the locale a bare language code most likely means on a phone line. Arabic
// has no single one; Egyptian Arabic is the most widely spoken.
var googleDefaultLocales = map[string]string{
	"ar": "ar-EG", "cs": "cs-CZ", "da": "da-DK", "de": "de-DE", "el": "el-GR", "en": "en-US",
	"es": "es-ES", "fi": "fi-FI", "fr": "fr-FR", "hi": "hi-IN", "hu": "hu-HU",
	"id": "id-ID", "it": "it-IT", "ja": "ja-JP", "ko": "ko-KR", "nl": "nl-NL",
	"pl": "pl-PL", "pt": "pt-BR", "ro": "ro-RO", "ru": "ru-RU", "sv": "sv-SE",
	"tr": "tr-TR", "uk": "uk-UA", "vi": "vi-VN", "zh": "cmn-Hans-CN",
}

// googleSpeechBaseURL resolves the API root: an explicit address verbatim
// (a stub, a proxy), otherwise the global host or the location's own.
func googleSpeechBaseURL(cfg googleCloudConfig, explicit string) string {
	if explicit != "" {
		return strings.TrimRight(explicit, "/")
	}
	if loc := cfg.location(); loc != vertexGlobalLocation {
		return "https://" + loc + "-speech.googleapis.com"
	}
	return "https://speech.googleapis.com"
}

// validateGoogleSpeechConfig reports why a Google Speech provider could
// never serve a request. Unlike Vertex an explicit address cannot stand in
// for the project: the project is part of every request's path, and it is
// the quota project billed.
func validateGoogleSpeechConfig(cfg googleCloudConfig) error {
	if cfg.Project == "" {
		return errors.New("google-speech provider needs a cloud project")
	}
	return nil
}
