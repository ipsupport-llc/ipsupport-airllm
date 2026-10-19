package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/llm"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/openai"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/providers"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/routing"
)

// handleEmbeddings implements the OpenAI POST /v1/embeddings ingress:
// policy gate, route resolution, provider call with fallback, and usage
// accounting — the same shape handleChatCompletions uses, without
// streaming (embeddings have no streaming wire format).
func (s *Server) handleEmbeddings(w http.ResponseWriter, r *http.Request) {
	ak, _ := keyFromContext(r.Context())
	start := time.Now()

	req, err := openai.DecodeEmbeddingsRequest(r.Body)
	if err != nil {
		writeProtocolError(w, r, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if !ak.Policy.Allows(req.Model) {
		writeProtocolError(w, r, http.StatusForbidden, "permission_error", "model not permitted for this key: "+req.Model)
		return
	}
	plan, err := s.router.Resolve(r.Context(), req.Model, ak.Policy.AllowPassthrough)
	if err != nil {
		writeProtocolError(w, r, http.StatusNotFound, "invalid_request_error", err.Error())
		return
	}
	if msg, denied, _ := s.limitDenied(r.Context(), ak, 0); denied {
		s.metrics.IncRateLimited("usage_limit")
		writeProtocolError(w, r, http.StatusTooManyRequests, "rate_limit_error", msg)
		return
	}

	// Pattern-scan (and redact or block) each input text before it leaves
	// the gateway, the same protection chat's own input gets and audio
	// already gives its transcript on the way back out — an embeddings
	// input is just as capable of carrying a secret to a third party.
	for i, text := range req.Input {
		blocked, msg, _, redacted := s.dlpScanText(r.Context(), ak, "openai", req.Model, text)
		if blocked {
			writeProtocolError(w, r, http.StatusBadRequest, "invalid_request_error", msg)
			return
		}
		req.Input[i] = redacted
	}

	resp, res, callErr := s.runEmbeddings(r.Context(), plan, req)
	entry := chatEntry(ak, req.Model, res, "openai", start)
	if callErr != nil {
		code, typ := classifyUpstreamErr(callErr)
		entry.Status = code
		entry.ErrorMsg = callErr.Error()
		s.finalizeUsage(r.Context(), entry, ak.KeyID, res.UpstreamModel, llm.Usage{}, 0, 0)
		writeProtocolError(w, r, code, typ, callErr.Error())
		return
	}

	resp.Model = req.Model
	entry.Status = http.StatusOK
	s.finalizeUsage(r.Context(), entry, ak.KeyID, res.UpstreamModel,
		llm.Usage{PromptTokens: resp.PromptTokens, TotalTokens: resp.PromptTokens}, 0, 0)

	body, err := openai.MarshalEmbeddingsResponse(resp)
	if err != nil {
		writeProtocolError(w, r, http.StatusInternalServerError, "internal_error", "failed to encode response")
		return
	}
	setBackendLabel(w, res.Target, plan.ExposeBackendHeaders)
	w.Header().Set("Content-Type", "application/json")
	w.Write(body)
}

// runEmbeddings executes the plan for an embeddings request (see
// executePlan), skipping targets whose provider cannot compute embeddings.
func (s *Server) runEmbeddings(ctx context.Context, plan *routing.Plan, req llm.EmbeddingsRequest) (llm.EmbeddingsResponse, execResult, error) {
	var resp llm.EmbeddingsResponse
	supports := func(p providers.Provider) error {
		if _, ok := p.(providers.Embedder); !ok {
			return &providers.Error{Status: http.StatusBadRequest, Retryable: false, Message: "provider " + p.Name() + " does not support embeddings"}
		}
		return nil
	}
	res, _, err := s.executePlan(ctx, plan, supports, func(ctx context.Context, p providers.Provider, t routing.Target, _ func() bool) error {
		in := req
		in.Model = t.UpstreamModel
		var err error
		resp, err = p.(providers.Embedder).Embeddings(ctx, in)
		return err
	})
	if err != nil {
		return llm.EmbeddingsResponse{}, res, err
	}
	return resp, res, nil
}
