package httpapi

import (
	"net/http"
	"time"

	"github.com/rromenskyi/ipsupport-airouter/internal/ledger"
	"github.com/rromenskyi/ipsupport-airouter/internal/llm"
	"github.com/rromenskyi/ipsupport-airouter/internal/openai"
)

// handleChatCompletions implements the OpenAI POST /v1/chat/completions
// ingress against a resolved provider, recording usage to the ledger.
//
// Phase 1: non-streaming only; routing is a stub (always the mock
// provider). Streaming and real routing land in later phases.
func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	ak, _ := keyFromContext(r.Context())
	start := time.Now()

	req, err := openai.DecodeChatRequest(r.Body)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if req.Stream {
		writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "streaming is not implemented yet")
		return
	}

	prov, upstreamModel := s.resolve(req.Model)

	resp, callErr := prov.Chat(r.Context(), llm.ChatRequest{
		Model:       upstreamModel,
		Messages:    req.Messages,
		Tools:       req.Tools,
		ToolChoice:  req.ToolChoice,
		Temperature: req.Temperature,
		MaxTokens:   req.MaxTokens,
	})

	entry := ledger.Entry{
		KeyID:            ak.KeyID,
		UserID:           ak.UserID,
		Alias:            req.Model,
		ProviderName:     prov.Name(),
		UpstreamModel:    upstreamModel,
		IngressProtocol:  "openai",
		UpstreamProtocol: prov.Protocol(),
		LatencyMS:        time.Since(start).Milliseconds(),
	}
	if callErr != nil {
		entry.Status = http.StatusBadGateway
		entry.ErrorMsg = callErr.Error()
		s.ledger.Record(r.Context(), entry)
		writeAPIError(w, http.StatusBadGateway, "upstream_error", callErr.Error())
		return
	}

	// Present the client-facing alias as the model name in the response.
	resp.Model = req.Model
	entry.Status = http.StatusOK
	entry.PromptTokens = resp.Usage.PromptTokens
	entry.CompletionTokens = resp.Usage.CompletionTokens
	s.ledger.Record(r.Context(), entry)

	body, err := openai.MarshalChatResponse(resp)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal_error", "failed to encode response")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// handleModels lists the model aliases as OpenAI model objects.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	rows, err := s.st.PG.Query(r.Context(), `SELECT alias FROM model_aliases ORDER BY alias`)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal_error", "failed to list models")
		return
	}
	defer rows.Close()

	type model struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		OwnedBy string `json:"owned_by"`
	}
	out := struct {
		Object string  `json:"object"`
		Data   []model `json:"data"`
	}{Object: "list", Data: []model{}}

	for rows.Next() {
		var alias string
		if err := rows.Scan(&alias); err != nil {
			writeAPIError(w, http.StatusInternalServerError, "internal_error", "failed to read models")
			return
		}
		out.Data = append(out.Data, model{ID: alias, Object: "model", OwnedBy: "airouter"})
	}
	if err := rows.Err(); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal_error", "failed to read models")
		return
	}
	writeJSON(w, http.StatusOK, out)
}
