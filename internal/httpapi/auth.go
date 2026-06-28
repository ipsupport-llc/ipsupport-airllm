package httpapi

import (
	"context"
	"net/http"
	"strings"

	"github.com/rromenskyi/ipsupport-airllm/internal/apikey"
	"github.com/rromenskyi/ipsupport-airllm/internal/policy"
)

type ctxKey int

const keyCtxKey ctxKey = iota

// authedKey is the API key resolved for a data-plane request.
type authedKey struct {
	KeyID  string
	UserID string
	Policy policy.KeyPolicy
}

// requireAPIKey authenticates a Bearer API key and stores the resolved key
// on the request context. It returns OpenAI-shaped errors on failure.
func (s *Server) requireAPIKey(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := bearerToken(r)
		if token == "" {
			writeProtocolError(w, r, http.StatusUnauthorized, "authentication_error", "missing API key")
			return
		}
		ak, err := s.lookupKey(r.Context(), token)
		if err != nil {
			writeProtocolError(w, r, http.StatusUnauthorized, "authentication_error", "invalid API key")
			return
		}
		ctx := context.WithValue(r.Context(), keyCtxKey, ak)
		next(w, r.WithContext(ctx))
	}
}

func (s *Server) lookupKey(ctx context.Context, token string) (authedKey, error) {
	var ak authedKey
	var raw []byte
	err := s.st.PG.QueryRow(ctx, `
		SELECT id::text, user_id::text, policy_snapshot
		FROM api_keys
		WHERE hash = $1 AND status = 'active'`,
		apikey.Hash(token),
	).Scan(&ak.KeyID, &ak.UserID, &raw)
	if err != nil {
		return authedKey{}, err
	}
	ak.Policy = policy.Parse(raw)
	// Best-effort last-used stamp; ignore failures.
	_, _ = s.st.PG.Exec(ctx, `UPDATE api_keys SET last_used_at = now() WHERE id = $1`, ak.KeyID)
	return ak, nil
}

func keyFromContext(ctx context.Context) (authedKey, bool) {
	ak, ok := ctx.Value(keyCtxKey).(authedKey)
	return ak, ok
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if h == "" {
		// Anthropic clients send the key in x-api-key.
		return strings.TrimSpace(r.Header.Get("x-api-key"))
	}
	const p = "Bearer "
	if len(h) > len(p) && strings.EqualFold(h[:len(p)], p) {
		return strings.TrimSpace(h[len(p):])
	}
	return ""
}

type apiError struct {
	Error apiErrorBody `json:"error"`
}

type apiErrorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
}

func writeAPIError(w http.ResponseWriter, code int, typ, msg string) {
	writeJSON(w, code, apiError{Error: apiErrorBody{Message: msg, Type: typ}})
}

// anthropicError is the Anthropic error envelope.
type anthropicError struct {
	Type  string             `json:"type"` // "error"
	Error anthropicErrorBody `json:"error"`
}

type anthropicErrorBody struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// writeProtocolError emits an error in the shape matching the ingress: the
// Anthropic envelope for /v1/messages, the OpenAI envelope otherwise.
func writeProtocolError(w http.ResponseWriter, r *http.Request, code int, openaiType, msg string) {
	if strings.HasPrefix(r.URL.Path, "/v1/messages") {
		writeJSON(w, code, anthropicError{
			Type:  "error",
			Error: anthropicErrorBody{Type: anthropicErrorType(code), Message: msg},
		})
		return
	}
	writeAPIError(w, code, openaiType, msg)
}

func anthropicErrorType(code int) string {
	switch code {
	case http.StatusBadRequest:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	default:
		return "api_error"
	}
}
