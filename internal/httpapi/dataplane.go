package httpapi

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
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
		s.streamChatCompletions(w, r, req, ak, start)
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

// streamChatCompletions serves the OpenAI Server-Sent Events stream for a
// chat completion. Once the 200 + first byte are written, errors can no
// longer be signalled to the client, so partial streams are only recorded
// to the ledger.
func (s *Server) streamChatCompletions(w http.ResponseWriter, r *http.Request, req llm.ChatRequest, ak authedKey, start time.Time) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeAPIError(w, http.StatusInternalServerError, "internal_error", "streaming unsupported")
		return
	}

	prov, upstreamModel := s.resolve(req.Model)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	meta := openai.StreamMeta{ID: "chatcmpl-" + newID(), Model: req.Model, Created: time.Now().Unix()}

	var usage llm.Usage
	streamErr := prov.ChatStream(r.Context(), llm.ChatRequest{
		Model:       upstreamModel,
		Messages:    req.Messages,
		Tools:       req.Tools,
		ToolChoice:  req.ToolChoice,
		Temperature: req.Temperature,
		MaxTokens:   req.MaxTokens,
		Stream:      true,
	}, func(c llm.StreamChunk) error {
		if c.Usage != nil {
			usage = *c.Usage
		}
		b, err := openai.MarshalStreamChunk(meta, c)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	})

	entry := ledger.Entry{
		KeyID:            ak.KeyID,
		UserID:           ak.UserID,
		Alias:            req.Model,
		ProviderName:     prov.Name(),
		UpstreamModel:    upstreamModel,
		IngressProtocol:  "openai",
		UpstreamProtocol: prov.Protocol(),
		PromptTokens:     usage.PromptTokens,
		CompletionTokens: usage.CompletionTokens,
		LatencyMS:        time.Since(start).Milliseconds(),
		Status:           http.StatusOK,
	}
	if streamErr != nil {
		entry.ErrorMsg = streamErr.Error()
		s.ledger.Record(r.Context(), entry)
		return
	}

	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
	s.ledger.Record(r.Context(), entry)
}

func newID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
