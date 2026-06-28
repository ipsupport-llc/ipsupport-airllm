package httpapi

import (
	"net/http"
	"time"

	"github.com/rromenskyi/ipsupport-airouter/internal/anthropic"
	"github.com/rromenskyi/ipsupport-airouter/internal/ledger"
	"github.com/rromenskyi/ipsupport-airouter/internal/llm"
)

// handleMessages implements the Anthropic POST /v1/messages ingress. The
// request is decoded into the IR, routed to a provider, and the IR response
// is encoded back into Anthropic shape.
func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	ak, _ := keyFromContext(r.Context())
	start := time.Now()

	req, err := anthropic.DecodeMessagesRequest(r.Body)
	if err != nil {
		writeProtocolError(w, r, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if req.Stream {
		s.streamMessages(w, r, req, ak, start)
		return
	}

	prov, upstreamModel := s.resolve(req.Model)
	resp, callErr := prov.Chat(r.Context(), upstreamRequest(req, upstreamModel))

	entry := ledger.Entry{
		KeyID:            ak.KeyID,
		UserID:           ak.UserID,
		Alias:            req.Model,
		ProviderName:     prov.Name(),
		UpstreamModel:    upstreamModel,
		IngressProtocol:  "anthropic",
		UpstreamProtocol: prov.Protocol(),
		LatencyMS:        time.Since(start).Milliseconds(),
	}
	if callErr != nil {
		entry.Status = http.StatusBadGateway
		entry.ErrorMsg = callErr.Error()
		s.ledger.Record(r.Context(), entry)
		writeProtocolError(w, r, http.StatusBadGateway, "upstream_error", callErr.Error())
		return
	}

	resp.Model = req.Model
	entry.Status = http.StatusOK
	entry.PromptTokens = resp.Usage.PromptTokens
	entry.CompletionTokens = resp.Usage.CompletionTokens
	s.ledger.Record(r.Context(), entry)

	body, err := anthropic.MarshalMessagesResponse(resp)
	if err != nil {
		writeProtocolError(w, r, http.StatusInternalServerError, "internal_error", "failed to encode response")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// streamMessages serves the Anthropic Messages SSE event stream.
func (s *Server) streamMessages(w http.ResponseWriter, r *http.Request, req llm.ChatRequest, ak authedKey, start time.Time) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeProtocolError(w, r, http.StatusInternalServerError, "internal_error", "streaming unsupported")
		return
	}

	prov, upstreamModel := s.resolve(req.Model)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	sw := anthropic.NewStreamWriter(w, flusher.Flush, "msg_"+newID(), req.Model, anthropic.EstimateInputTokens(req))

	var usage llm.Usage
	streamErr := prov.ChatStream(r.Context(), upstreamRequest(req, upstreamModel), func(c llm.StreamChunk) error {
		if c.Usage != nil {
			usage = *c.Usage
		}
		return sw.Chunk(c)
	})

	entry := ledger.Entry{
		KeyID:            ak.KeyID,
		UserID:           ak.UserID,
		Alias:            req.Model,
		ProviderName:     prov.Name(),
		UpstreamModel:    upstreamModel,
		IngressProtocol:  "anthropic",
		UpstreamProtocol: prov.Protocol(),
		PromptTokens:     usage.PromptTokens,
		CompletionTokens: usage.CompletionTokens,
		LatencyMS:        time.Since(start).Milliseconds(),
		Status:           http.StatusOK,
	}
	if streamErr != nil {
		entry.ErrorMsg = streamErr.Error()
	}
	s.ledger.Record(r.Context(), entry)
}

// upstreamRequest builds the provider-facing request with the resolved
// upstream model name substituted in.
func upstreamRequest(req llm.ChatRequest, upstreamModel string) llm.ChatRequest {
	out := req
	out.Model = upstreamModel
	return out
}
