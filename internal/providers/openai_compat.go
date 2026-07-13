package providers

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

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

func (p *OpenAICompat) newRequest(ctx context.Context, body []byte) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if p.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
	return req, nil
}

func httpError(name string, status int, body []byte) error {
	return &Error{
		Status:    status,
		Retryable: status == http.StatusTooManyRequests || status >= 500,
		Message:   fmt.Sprintf("upstream %s returned %d: %s", name, status, strings.TrimSpace(string(body))),
	}
}

// Chat performs a non-streaming upstream call.
func (p *OpenAICompat) Chat(ctx context.Context, in llm.ChatRequest) (llm.ChatResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()

	body, err := openai.EncodeChatRequest(in, false)
	if err != nil {
		return llm.ChatResponse{}, err
	}
	req, err := p.newRequest(ctx, body)
	if err != nil {
		return llm.ChatResponse{}, err
	}
	resp, err := p.hc.Do(req)
	if err != nil {
		return llm.ChatResponse{}, &Error{Status: http.StatusBadGateway, Retryable: true, Message: err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return llm.ChatResponse{}, httpError(p.name, resp.StatusCode, b)
	}
	return openai.DecodeChatResponse(resp.Body)
}

// ChatStream streams an upstream call, translating SSE chunks into the IR.
func (p *OpenAICompat) ChatStream(ctx context.Context, in llm.ChatRequest, yield func(llm.StreamChunk) error) error {
	body, err := openai.EncodeChatRequest(in, true)
	if err != nil {
		return err
	}
	req, err := p.newRequest(ctx, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")

	resp, err := p.hc.Do(req)
	if err != nil {
		return &Error{Status: http.StatusBadGateway, Retryable: true, Message: err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return httpError(p.name, resp.StatusCode, b)
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(line[len("data:"):])
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			break
		}
		chunk, err := openai.ParseStreamChunk([]byte(data))
		if err != nil {
			continue // skip a malformed chunk rather than abort the stream
		}
		if err := yield(chunk); err != nil {
			return err
		}
	}
	return sc.Err()
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
	var prices []ModelPrice
	for _, m := range out.Data {
		if m.ID == "" || m.Pricing == nil {
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
		prices = append(prices, ModelPrice{ID: m.ID, InputPer1M: inPer * 1e6, OutputPer1M: outPer * 1e6})
	}
	sort.Slice(prices, func(i, j int) bool { return prices[i].ID < prices[j].ID })
	return prices, nil
}
