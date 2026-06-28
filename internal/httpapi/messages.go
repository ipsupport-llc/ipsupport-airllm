package httpapi

import (
	"net/http"
	"time"

	"github.com/rromenskyi/ipsupport-airllm/internal/anthropic"
	"github.com/rromenskyi/ipsupport-airllm/internal/llm"
	"github.com/rromenskyi/ipsupport-airllm/internal/routing"
)

// handleMessages implements the Anthropic POST /v1/messages ingress: policy
// gate, route resolution, provider call with fallback, and usage accounting.
// The request is decoded into the IR and the IR response is encoded back into
// Anthropic shape.
func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	ak, _ := keyFromContext(r.Context())
	start := time.Now()

	req, err := anthropic.DecodeMessagesRequest(r.Body)
	if err != nil {
		writeProtocolError(w, r, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if !ak.Policy.Allows(req.Model) {
		writeProtocolError(w, r, http.StatusForbidden, "permission_error", "model not permitted for this key: "+req.Model)
		return
	}
	targets, err := s.router.Resolve(r.Context(), req.Model, ak.Policy.AllowPassthrough)
	if err != nil {
		writeProtocolError(w, r, http.StatusNotFound, "invalid_request_error", err.Error())
		return
	}
	if msg, denied := s.limitDenied(r.Context(), ak); denied {
		writeProtocolError(w, r, http.StatusTooManyRequests, "rate_limit_error", msg)
		return
	}

	if req.Stream {
		s.streamMessages(w, r, req, ak, start, targets)
		return
	}

	resp, target, callErr := s.runChat(r.Context(), targets, req)
	entry := chatEntry(ak, req.Model, target, "anthropic", start)
	if callErr != nil {
		entry.Status = http.StatusBadGateway
		entry.ErrorMsg = callErr.Error()
		s.finalizeUsage(r.Context(), entry, ak.KeyID, target.UpstreamModel, 0, 0)
		writeProtocolError(w, r, http.StatusBadGateway, "upstream_error", callErr.Error())
		return
	}

	resp.Model = req.Model
	entry.Status = http.StatusOK
	s.finalizeUsage(r.Context(), entry, ak.KeyID, target.UpstreamModel, resp.Usage.PromptTokens, resp.Usage.CompletionTokens)

	body, err := anthropic.MarshalMessagesResponse(resp)
	if err != nil {
		writeProtocolError(w, r, http.StatusInternalServerError, "internal_error", "failed to encode response")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func (s *Server) streamMessages(w http.ResponseWriter, r *http.Request, req llm.ChatRequest, ak authedKey, start time.Time, targets []routing.Target) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeProtocolError(w, r, http.StatusInternalServerError, "internal_error", "streaming unsupported")
		return
	}

	sink := &anthropicSink{
		w: w,
		sw: anthropic.NewStreamWriter(w, flusher.Flush,
			"msg_"+newID(), req.Model, anthropic.EstimateInputTokens(req)),
	}

	target, usage, started, err := s.runStream(r.Context(), targets, req, sink)
	entry := chatEntry(ak, req.Model, target, "anthropic", start)

	if err != nil {
		entry.ErrorMsg = err.Error()
		if !started {
			entry.Status = http.StatusBadGateway
			s.finalizeUsage(r.Context(), entry, ak.KeyID, target.UpstreamModel, 0, 0)
			writeProtocolError(w, r, http.StatusBadGateway, "upstream_error", err.Error())
			return
		}
		entry.Status = http.StatusOK
		s.finalizeUsage(r.Context(), entry, ak.KeyID, target.UpstreamModel, usage.PromptTokens, usage.CompletionTokens)
		return
	}

	entry.Status = http.StatusOK
	s.finalizeUsage(r.Context(), entry, ak.KeyID, target.UpstreamModel, usage.PromptTokens, usage.CompletionTokens)
}

// anthropicSink streams Anthropic Messages SSE events via a StreamWriter.
type anthropicSink struct {
	w  http.ResponseWriter
	sw *anthropic.StreamWriter
}

func (a *anthropicSink) begin() {
	writeSSEHeaders(a.w)
}

func (a *anthropicSink) chunk(c llm.StreamChunk) error {
	return a.sw.Chunk(c)
}
