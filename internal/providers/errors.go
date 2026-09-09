package providers

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

// Known error codes that make a failure fallback-worthy even though it is
// not retryable against the same target — see IsFallbackWorthy.
const (
	ErrCodeContextLengthExceeded      = "context_length_exceeded"
	ErrCodeModelNotFound              = "model_not_found"
	ErrCodeMultimodalNotSupported     = "multimodal_not_supported"
	ErrCodeReasoningEffortUnsupported = "reasoning_effort_unsupported"
)

// Error is a provider call failure. Retryable failures (e.g. upstream 429 or
// 5xx) let the router fall back to the next target; non-retryable failures
// (e.g. a bad request) abort — UNLESS Code names a known fallback-worthy
// reason (see IsFallbackWorthy), in which case the router still tries the
// next tier even though retrying the SAME target would not help.
type Error struct {
	Status    int
	Retryable bool
	Code      string
	Message   string
}

func (e *Error) Error() string { return e.Message }

// IsRetryable reports whether err is a retryable provider Error.
func IsRetryable(err error) bool {
	var pe *Error
	if errors.As(err, &pe) {
		return pe.Retryable
	}
	return false
}

// IsFallbackWorthy reports whether the router should try the next
// priority tier after this error, rather than aborting the whole request.
// True for every retryable error (unchanged meaning), plus a short list of
// known error codes that mean "this specific target can't serve this
// request" (e.g. its context window is too small, or its model was
// removed) rather than "this request is malformed."
func IsFallbackWorthy(err error) bool {
	if IsRetryable(err) {
		return true
	}
	var pe *Error
	if errors.As(err, &pe) {
		switch pe.Code {
		case ErrCodeContextLengthExceeded, ErrCodeModelNotFound, ErrCodeMultimodalNotSupported, ErrCodeReasoningEffortUnsupported:
			return true
		}
	}
	return false
}

// openAIErrorBody is the error shape OpenAI itself, and every OpenAI-compatible
// cloud vendor this codebase talks to (Groq, xAI, OpenRouter), uses.
type openAIErrorBody struct {
	Error struct {
		Type    string `json:"type"`
		Code    string `json:"code"`
		Message string `json:"message"`
		Param   string `json:"param"`
	} `json:"error"`
}

// ollamaMultimodalRejectionPattern matches Ollama's OpenAI-compat shim
// rejecting an image sent to a non-vision model. Unlike the codes above,
// this one has no stable machine-readable signal anywhere in the response:
// Ollama's outer envelope arrives with code:null and a generic type
// ("invalid_request_error", same as many unrelated errors), and the real
// error is itself double-nested — a JSON-encoded string sitting inside the
// outer message field, whose own type is just as generic. The literal
// English text is the only thing that distinguishes this error, so this
// matches directly against the decoded outer message string (which already
// contains that text verbatim, nested-JSON escaping and all) rather than
// unmarshaling the inner JSON separately. Fragile to Ollama rewording this
// message in a future version — known and accepted, there is nothing more
// precise to match on.
var ollamaMultimodalRejectionPattern = regexp.MustCompile(`(?i)does not support multimodal`)

// llamaCppErrorBody is the error shape llama.cpp's server (and Ollama, which
// wraps it) uses — distinct from the OpenAI shape: the reason lives in
// error.type, not error.code, and there is no error.code field at all.
type llamaCppErrorBody struct {
	Error struct {
		Type string `json:"type"`
	} `json:"error"`
}

// googleErrorBody is the error shape every Google API uses, Vertex AI's
// OpenAI-compatible surface included. It shares the `error` wrapper with the
// OpenAI shape and nothing inside it: the code is numeric where OpenAI's is a
// string, and the reason it carries is a status enum.
type googleErrorBody struct {
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}

// googleContextLimitHints are the phrases Google uses when an INVALID_ARGUMENT
// really means "the prompt is longer than the model's window". There is no
// distinct status for it — the message is the only signal there is — so this
// list is a best effort that fails safe: a phrasing it misses leaves the
// error unclassified, exactly as it was before.
var googleContextLimitHints = []string{
	"token count",
	"maximum number of tokens",
	"token limit",
	"context length",
	"input is too long",
}

// classifyErrorBody does best-effort parsing of a non-2xx response body
// against the known vendor error shapes, returning a recognized providers
// error code or "" if none matches or matches something we don't
// specifically track. A body that doesn't match any shape — including one
// where a field arrives as the wrong JSON type, e.g. llama.cpp's and
// Google's numeric `code` failing to unmarshal into openAIErrorBody's
// string field — simply leaves Code empty. A JSON `code: null` is a
// different case: Go's encoding/json decodes a JSON null into a
// zero-value string without erroring, so that attempt "succeeds" with an
// empty Code, which then simply misses the switch below and falls through
// to the next shape. Both paths converge on the same safe outcome (Code
// stays empty), just via different mechanisms — worth knowing precisely,
// not just that it's "safe."
//
// The shapes are tried in the order they were added, and each later one only
// ever sees a body the earlier ones declined: that is what keeps a new vendor
// from changing how an old vendor's envelope is classified.
func classifyErrorBody(body []byte) string {
	var oa openAIErrorBody
	if err := json.Unmarshal(body, &oa); err == nil {
		switch oa.Error.Code {
		case ErrCodeContextLengthExceeded, ErrCodeModelNotFound:
			return oa.Error.Code
		}
		// OpenAI rejects some newer reasoning models' combination of tool
		// calls with a client-supplied reasoning_effort via this endpoint
		// (its own message points at /v1/responses instead, which this
		// codebase doesn't speak) — a real, narrow model limitation, not a
		// malformed request, so another tier is worth trying. `param` is a
		// genuine structured signal here, unlike the Ollama case below.
		if oa.Error.Param == "reasoning_effort" {
			return ErrCodeReasoningEffortUnsupported
		}
		if ollamaMultimodalRejectionPattern.MatchString(oa.Error.Message) {
			return ErrCodeMultimodalNotSupported
		}
	}
	var lc llamaCppErrorBody
	if err := json.Unmarshal(body, &lc); err == nil {
		if lc.Error.Type == "exceed_context_size_error" {
			return ErrCodeContextLengthExceeded
		}
	}
	var g googleErrorBody
	if err := json.Unmarshal(body, &g); err == nil {
		switch g.Error.Status {
		case "NOT_FOUND":
			// An unknown publisher model. Fallback-worthy: another tier may
			// well have the model this one doesn't.
			return ErrCodeModelNotFound
		case "INVALID_ARGUMENT":
			msg := strings.ToLower(g.Error.Message)
			for _, hint := range googleContextLimitHints {
				if strings.Contains(msg, hint) {
					return ErrCodeContextLengthExceeded
				}
			}
		}
	}
	return ""
}

// httpError builds a provider Error from a non-2xx upstream response.
func httpError(name string, status int, body []byte) error {
	return &Error{
		Status:    status,
		Retryable: status == http.StatusTooManyRequests || status >= 500,
		Code:      classifyErrorBody(body),
		Message:   fmt.Sprintf("upstream %s returned %d: %s", name, status, strings.TrimSpace(string(body))),
	}
}
