package providers

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
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
