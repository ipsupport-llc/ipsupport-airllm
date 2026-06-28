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
	"github.com/rromenskyi/ipsupport-airouter/internal/routing"
)

// handleChatCompletions implements the OpenAI POST /v1/chat/completions
// ingress: policy gate, route resolution, provider call with fallback, and
// usage accounting.
func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	ak, _ := keyFromContext(r.Context())
	start := time.Now()

	req, err := openai.DecodeChatRequest(r.Body)
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
		s.streamChatCompletions(w, r, req, ak, start, targets)
		return
	}

	resp, target, callErr := s.runChat(r.Context(), targets, req)
	entry := chatEntry(ak, req.Model, target, "openai", start)
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

	body, err := openai.MarshalChatResponse(resp)
	if err != nil {
		writeProtocolError(w, r, http.StatusInternalServerError, "internal_error", "failed to encode response")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func (s *Server) streamChatCompletions(w http.ResponseWriter, r *http.Request, req llm.ChatRequest, ak authedKey, start time.Time, targets []routing.Target) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeProtocolError(w, r, http.StatusInternalServerError, "internal_error", "streaming unsupported")
		return
	}

	sink := &openaiSink{
		w:     w,
		flush: flusher.Flush,
		meta:  openai.StreamMeta{ID: "chatcmpl-" + newID(), Model: req.Model, Created: time.Now().Unix()},
	}

	target, usage, started, err := s.runStream(r.Context(), targets, req, sink)
	entry := chatEntry(ak, req.Model, target, "openai", start)

	if err != nil {
		entry.ErrorMsg = err.Error()
		if !started {
			entry.Status = http.StatusBadGateway
			s.finalizeUsage(r.Context(), entry, ak.KeyID, target.UpstreamModel, 0, 0)
			writeProtocolError(w, r, http.StatusBadGateway, "upstream_error", err.Error())
			return
		}
		entry.Status = http.StatusOK // headers already sent; cannot signal failure
		s.finalizeUsage(r.Context(), entry, ak.KeyID, target.UpstreamModel, usage.PromptTokens, usage.CompletionTokens)
		return
	}

	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
	entry.Status = http.StatusOK
	s.finalizeUsage(r.Context(), entry, ak.KeyID, target.UpstreamModel, usage.PromptTokens, usage.CompletionTokens)
}

// handleModels lists the model aliases as OpenAI model objects.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	rows, err := s.st.PG.Query(r.Context(), `SELECT alias FROM model_aliases ORDER BY alias`)
	if err != nil {
		writeProtocolError(w, r, http.StatusInternalServerError, "internal_error", "failed to list models")
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
			writeProtocolError(w, r, http.StatusInternalServerError, "internal_error", "failed to read models")
			return
		}
		out.Data = append(out.Data, model{ID: alias, Object: "model", OwnedBy: "airouter"})
	}
	if err := rows.Err(); err != nil {
		writeProtocolError(w, r, http.StatusInternalServerError, "internal_error", "failed to read models")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// chatEntry seeds a ledger entry common to the chat and messages paths.
func chatEntry(ak authedKey, alias string, t routing.Target, ingress string, start time.Time) ledger.Entry {
	return ledger.Entry{
		KeyID:            ak.KeyID,
		UserID:           ak.UserID,
		Alias:            alias,
		ProviderName:     t.Provider,
		UpstreamModel:    t.UpstreamModel,
		IngressProtocol:  ingress,
		UpstreamProtocol: t.UpstreamProtocol,
		LatencyMS:        time.Since(start).Milliseconds(),
	}
}

func newID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
