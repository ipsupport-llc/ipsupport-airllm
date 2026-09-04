package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/audio"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/llm"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/openai"
)

// OpenAICompat is a real upstream that speaks the OpenAI chat-completions API:
// OpenAI, OpenRouter, xAI (Grok), and Ollama. They differ only by base URL and
// API key.
type OpenAICompat struct {
	name    string
	kind    string
	baseURL string // e.g. https://api.openai.com/v1 (no trailing slash)
	apiKey  string
	hc      *http.Client
}

// NewOpenAICompat builds an OpenAI-compatible provider.
func NewOpenAICompat(name, kind, baseURL, apiKey string) *OpenAICompat {
	return &OpenAICompat{
		name:    name,
		kind:    kind,
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		hc:      &http.Client{},
	}
}

func (p *OpenAICompat) Name() string     { return p.name }
func (p *OpenAICompat) Kind() string     { return p.kind }
func (p *OpenAICompat) Protocol() string { return "openai" }

// audioHTTPError wraps httpError with a hint for 404, the most common
// signature of a real, non-mock provider that doesn't actually implement
// the OpenAI audio API — every OpenAICompat instance structurally passes
// the Transcriber/Synthesizer type assertion regardless of whether the
// configured vendor/base_url has these endpoints, so a misconfigured audio
// alias reaches this point instead of failing a local capability check.
func audioHTTPError(name string, status int, body []byte) error {
	err := httpError(name, status, body)
	if status == http.StatusNotFound {
		if pe, ok := err.(*Error); ok {
			pe.Message += " (this provider/model may not support the OpenAI audio API)"
		}
	}
	return err
}

// Chat performs a non-streaming upstream call.
func (p *OpenAICompat) Chat(ctx context.Context, in llm.ChatRequest) (llm.ChatResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()

	body, err := openai.EncodeChatRequest(in, false)
	if err != nil {
		return llm.ChatResponse{}, err
	}
	resp, err := sendChatCompletions(ctx, p.hc, p.name, p.baseURL, p.apiKey, body, false)
	if err != nil {
		return llm.ChatResponse{}, err
	}
	defer resp.Body.Close()
	return openai.DecodeChatResponse(resp.Body)
}

// ChatStream streams an upstream call, translating SSE chunks into the IR.
func (p *OpenAICompat) ChatStream(ctx context.Context, in llm.ChatRequest, yield func(llm.StreamChunk) error) error {
	body, err := openai.EncodeChatRequest(in, true)
	if err != nil {
		return err
	}
	if debugUpstreamSSE {
		slog.Info("upstream request", "provider", p.name, "body", string(body))
	}
	resp, err := sendChatCompletions(ctx, p.hc, p.name, p.baseURL, p.apiKey, body, true)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	return decodeSSEStream(resp.Body, streamDecodeOptions{provider: p.name}, yield)
}

// ListModels fetches GET {base}/models and returns the sorted, de-duplicated
// model ids. All OpenAI-compatible upstreams expose this endpoint.
func (p *OpenAICompat) ListModels(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/models", nil)
	if err != nil {
		return nil, err
	}
	if p.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
	resp, err := p.hc.Do(req)
	if err != nil {
		return nil, &Error{Status: http.StatusBadGateway, Retryable: true, Message: err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, httpError(p.name, resp.StatusCode, b)
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode %s models: %w", p.name, err)
	}
	seen := map[string]bool{}
	var ids []string
	for _, m := range out.Data {
		if m.ID == "" || seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		ids = append(ids, m.ID)
	}
	sort.Strings(ids)
	return ids, nil
}

// ListModelPricing fetches GET {base}/models and returns the catalog entries
// that publish pricing (OpenRouter's `pricing.prompt`/`pricing.completion`,
// USD per token as strings). Prices are converted to USD per 1M tokens.
// Entries with a missing or unparseable pricing object are skipped. Results
// are sorted by id.
func (p *OpenAICompat) ListModelPricing(ctx context.Context) ([]ModelPrice, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/models", nil)
	if err != nil {
		return nil, err
	}
	if p.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
	resp, err := p.hc.Do(req)
	if err != nil {
		return nil, &Error{Status: http.StatusBadGateway, Retryable: true, Message: err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, httpError(p.name, resp.StatusCode, b)
	}
	var out struct {
		Data []struct {
			ID      string `json:"id"`
			Pricing *struct {
				Prompt     string `json:"prompt"`
				Completion string `json:"completion"`
			} `json:"pricing"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode %s models: %w", p.name, err)
	}
	seen := map[string]bool{}
	var prices []ModelPrice
	for _, m := range out.Data {
		if m.ID == "" || m.Pricing == nil || seen[m.ID] {
			continue
		}
		inPer, err := strconv.ParseFloat(m.Pricing.Prompt, 64)
		if err != nil {
			continue
		}
		outPer, err := strconv.ParseFloat(m.Pricing.Completion, 64)
		if err != nil {
			continue
		}
		inPer1M := inPer * 1e6
		outPer1M := outPer * 1e6
		// pricing columns are numeric(12,4), which holds values < 10^8;
		// treat anything outside that range as garbage and skip it rather
		// than let the import tx abort on an overflow.
		if inPer1M < 0 || inPer1M >= 1e8 || outPer1M < 0 || outPer1M >= 1e8 {
			continue
		}
		seen[m.ID] = true
		prices = append(prices, ModelPrice{ID: m.ID, InputPer1M: inPer1M, OutputPer1M: outPer1M})
	}
	sort.Slice(prices, func(i, j int) bool { return prices[i].ID < prices[j].ID })
	return prices, nil
}

// Transcribe uploads audio for transcription. It always requests
// response_format=verbose_json upstream — regardless of what the
// gateway's own client asked for — because the gateway needs the
// reported duration for pricing.
func (p *OpenAICompat) Transcribe(ctx context.Context, in audio.TranscriptionRequest) (audio.TranscriptionResponse, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("model", in.Model); err != nil {
		return audio.TranscriptionResponse{}, err
	}
	if err := mw.WriteField("response_format", "verbose_json"); err != nil {
		return audio.TranscriptionResponse{}, err
	}
	if in.Language != "" {
		if err := mw.WriteField("language", in.Language); err != nil {
			return audio.TranscriptionResponse{}, err
		}
	}
	if in.Prompt != "" {
		if err := mw.WriteField("prompt", in.Prompt); err != nil {
			return audio.TranscriptionResponse{}, err
		}
	}
	fw, err := mw.CreateFormFile("file", in.Filename)
	if err != nil {
		return audio.TranscriptionResponse{}, err
	}
	if _, err := fw.Write(in.Audio); err != nil {
		return audio.TranscriptionResponse{}, err
	}
	if err := mw.Close(); err != nil {
		return audio.TranscriptionResponse{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/audio/transcriptions", &buf)
	if err != nil {
		return audio.TranscriptionResponse{}, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if p.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.apiKey)
	}

	resp, err := p.hc.Do(req)
	if err != nil {
		return audio.TranscriptionResponse{}, &Error{Status: http.StatusBadGateway, Retryable: true, Message: err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return audio.TranscriptionResponse{}, audioHTTPError(p.name, resp.StatusCode, b)
	}

	var w struct {
		Text     string  `json:"text"`
		Duration float64 `json:"duration"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&w); err != nil {
		return audio.TranscriptionResponse{}, err
	}
	return audio.TranscriptionResponse{Text: w.Text, DurationSeconds: w.Duration}, nil
}

// Synthesize requests text-to-speech audio. The response is read whole —
// batch only, no streaming — and the upstream Content-Type is passed
// through so the client gets the right audio format.
func (p *OpenAICompat) Synthesize(ctx context.Context, in audio.SpeechRequest) (audio.SpeechResponse, error) {
	body, err := json.Marshal(struct {
		Model          string `json:"model"`
		Input          string `json:"input"`
		Voice          string `json:"voice"`
		ResponseFormat string `json:"response_format,omitempty"`
	}{Model: in.Model, Input: in.Input, Voice: in.Voice, ResponseFormat: in.ResponseFormat})
	if err != nil {
		return audio.SpeechResponse{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/audio/speech", bytes.NewReader(body))
	if err != nil {
		return audio.SpeechResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if p.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.apiKey)
	}

	resp, err := p.hc.Do(req)
	if err != nil {
		return audio.SpeechResponse{}, &Error{Status: http.StatusBadGateway, Retryable: true, Message: err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return audio.SpeechResponse{}, audioHTTPError(p.name, resp.StatusCode, b)
	}
	audioBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return audio.SpeechResponse{}, err
	}
	return audio.SpeechResponse{Audio: audioBytes, ContentType: resp.Header.Get("Content-Type")}, nil
}
