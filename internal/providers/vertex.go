package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
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
//
// Being curated, it is a statement of intent rather than a mirror of the
// catalogue: an id belongs here only once Vertex answers to it and it is
// priced, and a superseded id comes out rather than staying on offer, as the
// 2.5 family did ahead of its 2026-10-20 retirement. docs/operations.md
// (Vertex AI prices) records why each id is or is not here, and its rates.
var vertexCuratedModels = []string{
	"google/gemini-3.1-flash-lite",
	"google/gemini-3.1-pro-preview",
	"google/gemini-3.5-flash",
	"google/gemini-3.5-flash-lite",
	"google/gemini-3.8-flash",
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
// A token-source failure is reported as retryable rather than fatal (via
// transportError): the exchange that failed once will very likely work
// again, and meanwhile another tier can serve this request. Treating it as
// fatal would take the whole alias down for a transient metadata-server
// hiccup. The one exception transportError carves out — the caller's own
// context being canceled — applies here too: a client that disconnected
// while this call was waiting on a token isn't helped by another tier
// either.
func (p *Vertex) bearer(ctx context.Context) (string, error) {
	token, err := p.tokens.Token(ctx)
	if err != nil {
		return "", transportError(fmt.Errorf("upstream %s token source: %w", p.name, err))
	}
	return token, nil
}

// vertexRequest qualifies the model id for the wire, leaving the caller's
// request — and so the ledger's spelling of the model — untouched. For a
// Gemini model it also makes sure every assistant step's first function call
// carries a thought signature, which Gemini 3 refuses the request without.
func vertexRequest(in llm.ChatRequest) llm.ChatRequest {
	out := in
	out.Model = normalizeVertexModel(in.Model)
	if !strings.HasPrefix(out.Model, "google/") {
		return out
	}
	return rewriteToolCalls(out, func(calls []llm.ToolCall) []llm.ToolCall {
		if len(calls) == 0 || hasThoughtSignature(calls[0]) {
			return nil
		}
		signed := slices.Clone(calls)
		signed[0].ExtraContent = withThoughtSignature(calls[0].ExtraContent, skipThoughtSignature)
		return signed
	})
}

// skipThoughtSignature is Google's documented stand-in for function calls
// that a model without signatures produced: a step served by a fallback tier,
// a client that does not echo extra_content, or Anthropic ingress, which has
// nowhere to carry it. Google calls it a last resort that costs reasoning
// quality, so a real signature always wins. Without it, each of those
// requests would be a 400 that aborts rather than falls back.
const skipThoughtSignature = "skip_thought_signature_validator"

// geminiExtraContent is the part of a tool call's extra_content that Gemini
// reads.
type geminiExtraContent struct {
	Google struct {
		ThoughtSignature string `json:"thought_signature"`
	} `json:"google"`
}

// hasThoughtSignature reports whether a tool call carries Gemini's thought
// signature. Gemini signs only the first call of a parallel step, so this is
// asked of that call alone.
func hasThoughtSignature(tc llm.ToolCall) bool {
	var extra geminiExtraContent
	return json.Unmarshal(tc.ExtraContent, &extra) == nil && extra.Google.ThoughtSignature != ""
}

// withThoughtSignature returns extra with google.thought_signature set to
// sig, keeping every other key. Anything that is not a JSON object is
// replaced, since Vertex could not have read it anyway.
func withThoughtSignature(extra json.RawMessage, sig string) json.RawMessage {
	var top, google map[string]json.RawMessage
	if json.Unmarshal(extra, &top) != nil || top == nil {
		top = map[string]json.RawMessage{}
	}
	if json.Unmarshal(top["google"], &google) != nil || google == nil {
		google = map[string]json.RawMessage{}
	}
	google["thought_signature"], _ = json.Marshal(sig)
	top["google"], _ = json.Marshal(google)
	b, _ := json.Marshal(top)
	return b
}

// Chat performs a non-streaming upstream call. The two-minute ceiling is
// parity with OpenAICompat; a large model with a generous thinking budget can
// approach it.
func (p *Vertex) Chat(ctx context.Context, in llm.ChatRequest) (llm.ChatResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()

	body, err := openai.EncodeChatRequest(vertexRequest(withThinkingOff(p.Kind(), in)), false)
	if err != nil {
		return llm.ChatResponse{}, err
	}
	token, err := p.bearer(ctx)
	if err != nil {
		return llm.ChatResponse{}, err
	}
	resp, err := sendChatCompletions(ctx, p.hc, p.name, p.baseURL, token, body, false)
	if err != nil {
		return llm.ChatResponse{}, thinkingRejection(p.Kind(), in, err)
	}
	defer resp.Body.Close()
	return openai.DecodeChatResponse(resp.Body)
}

// ChatStream streams an upstream call. Usage is coalesced: Vertex reports
// cumulative totals on many chunks, and forwarding each one would end an
// Anthropic-shaped stream early and then repeatedly.
func (p *Vertex) ChatStream(ctx context.Context, in llm.ChatRequest, yield func(llm.StreamChunk) error) error {
	body, err := openai.EncodeChatRequest(vertexRequest(withThinkingOff(p.Kind(), in)), true)
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
		return thinkingRejection(p.Kind(), in, err)
	}
	defer resp.Body.Close()

	return decodeSSEStream(resp.Body, streamDecodeOptions{provider: p.name, coalesceUsage: true}, yield)
}
