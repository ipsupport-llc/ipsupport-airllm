package providers

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/llm"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/openai"
)

// Vertex is Google Vertex AI, reached over its OpenAI-compatible chat surface
// rather than its native generate-content protocol. The decisive reason for
// that choice is tool-call identity: the IR requires ids on tool calls, the
// native protocol has none and correlates by name, and the compatibility
// layer synthesises and correlates them server-side. Owning that correlation
// here would mean owning its bugs.
//
// It is a distinct type rather than another kind handled by OpenAICompat, for
// three independent reasons, any one of which would be enough:
//
//   - Its credential is a short-lived OAuth2 token consulted per request, not
//     a static key captured once when the registry is built.
//   - Vertex reports *cumulative* usage on many stream chunks, so its stream
//     is decoded with coalescing on. The compatible kinds forward every usage
//     report, and the Anthropic-shaped egress ends the message on each one.
//   - OpenAICompat structurally satisfies Transcriber and Synthesizer whether
//     or not the configured vendor implements the OpenAI audio API. Embedding
//     it would let an audio alias resolve here and fail at request time.
//     Vertex holds its pieces as plain fields and embeds nothing, so it
//     declares only chat, streaming and model listing — pinned down by
//     TestVertexDeclaresNoAudioCapability, because a later refactor could
//     otherwise quietly re-open that misconfiguration.
type Vertex struct {
	name    string
	baseURL string // .../endpoints/openapi (no trailing slash)
	tokens  TokenSource
	hc      *http.Client
}

// NewVertex builds a Vertex provider addressed at baseURL — see
// VertexBaseURL, which assembles it from the provider's configuration —
// authenticating with tokens.
func NewVertex(name, baseURL string, tokens TokenSource) *Vertex {
	return &Vertex{
		name:    name,
		baseURL: strings.TrimRight(baseURL, "/"),
		tokens:  tokens,
		hc:      &http.Client{},
	}
}

func (p *Vertex) Name() string     { return p.name }
func (p *Vertex) Kind() string     { return "vertex" }
func (p *Vertex) Protocol() string { return "openai" }

// vertexCuratedModels is a hand-maintained list, NOT a live catalogue: the
// OpenAI-compatible surface publishes no model list and Vertex's own
// catalogue speaks a different protocol, so there is nothing to fetch. Its
// only consumer is the alias editor's dropdown — the data-plane model list
// returns aliases and is unaffected — so a model missing here can still be
// typed in by hand.
var vertexCuratedModels = []string{
	"google/gemini-2.5-flash",
	"google/gemini-2.5-flash-lite",
	"google/gemini-2.5-pro",
	"google/gemini-3-flash",
	"google/gemini-3-pro",
}

// ListModels returns the curated model ids. Vertex deliberately does not
// implement PricedModelLister: Google publishes no machine-readable price
// list for these models, and a provider that pretends otherwise would import
// nothing while looking like it worked. Vertex prices are entered by hand.
func (p *Vertex) ListModels(context.Context) ([]string, error) {
	return append([]string(nil), vertexCuratedModels...), nil
}

// bearer mints the access token for one call.
//
// A token-source failure is reported as retryable rather than fatal: the
// exchange that failed once will very likely work again, and meanwhile
// another tier can serve this request. Treating it as fatal would take the
// whole alias down for a transient metadata-server hiccup.
func (p *Vertex) bearer(ctx context.Context) (string, error) {
	token, err := p.tokens.Token(ctx)
	if err != nil {
		return "", &Error{
			Status:    http.StatusBadGateway,
			Retryable: true,
			Message:   fmt.Sprintf("upstream %s token source: %v", p.name, err),
		}
	}
	return token, nil
}

// vertexRequest qualifies the model id for the wire, leaving the caller's
// request — and so the ledger's spelling of the model — untouched.
func vertexRequest(in llm.ChatRequest) llm.ChatRequest {
	out := in
	out.Model = normalizeVertexModel(in.Model)
	return out
}

// Chat performs a non-streaming upstream call. The two-minute ceiling is
// parity with OpenAICompat; a large model with a generous thinking budget can
// approach it.
func (p *Vertex) Chat(ctx context.Context, in llm.ChatRequest) (llm.ChatResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()

	body, err := openai.EncodeChatRequest(vertexRequest(in), false)
	if err != nil {
		return llm.ChatResponse{}, err
	}
	token, err := p.bearer(ctx)
	if err != nil {
		return llm.ChatResponse{}, err
	}
	resp, err := sendChatCompletions(ctx, p.hc, p.name, p.baseURL, token, body, false)
	if err != nil {
		return llm.ChatResponse{}, err
	}
	defer resp.Body.Close()
	return openai.DecodeChatResponse(resp.Body)
}

// ChatStream streams an upstream call. Usage is coalesced: Vertex reports
// cumulative totals on many chunks, and forwarding each one would end an
// Anthropic-shaped stream early and then repeatedly.
func (p *Vertex) ChatStream(ctx context.Context, in llm.ChatRequest, yield func(llm.StreamChunk) error) error {
	body, err := openai.EncodeChatRequest(vertexRequest(in), true)
	if err != nil {
		return err
	}
	if debugUpstreamSSE {
		slog.Info("upstream request", "provider", p.name, "body", string(body))
	}
	token, err := p.bearer(ctx)
	if err != nil {
		return err
	}
	resp, err := sendChatCompletions(ctx, p.hc, p.name, p.baseURL, token, body, true)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	return decodeSSEStream(resp.Body, streamDecodeOptions{provider: p.name, coalesceUsage: true}, yield)
}
