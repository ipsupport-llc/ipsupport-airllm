package providers

import (
	"errors"
	"testing"
)

func TestIsFallbackWorthyRetryable(t *testing.T) {
	err := &Error{Status: 503, Retryable: true}
	if !IsFallbackWorthy(err) {
		t.Error("a retryable error must be fallback-worthy")
	}
}

func TestIsFallbackWorthyKnownCode(t *testing.T) {
	for _, code := range []string{ErrCodeContextLengthExceeded, ErrCodeModelNotFound} {
		err := &Error{Status: 400, Retryable: false, Code: code}
		if !IsFallbackWorthy(err) {
			t.Errorf("code %q must be fallback-worthy even though Retryable=false", code)
		}
	}
}

func TestIsFallbackWorthyUnknownCode(t *testing.T) {
	err := &Error{Status: 400, Retryable: false, Code: ""}
	if IsFallbackWorthy(err) {
		t.Error("a non-retryable error with no recognized code must not be fallback-worthy")
	}
	err2 := &Error{Status: 400, Retryable: false, Code: "some_other_code"}
	if IsFallbackWorthy(err2) {
		t.Error("a non-retryable error with an unrecognized code must not be fallback-worthy")
	}
}

func TestIsFallbackWorthyNonProviderError(t *testing.T) {
	if IsFallbackWorthy(errors.New("plain error")) {
		t.Error("a plain non-*Error must not be fallback-worthy, matching IsRetryable's existing behavior")
	}
}

// TestClassifyErrorBody covers all three vendor error envelopes in one table.
// Google's arrival is the reason it exists: its envelope shares the `error`
// wrapper with OpenAI's but types `code` as a number, so the risk worth
// pinning down is not only that the new shape is recognized but that the two
// that were already handled still classify exactly as they did.
func TestClassifyErrorBody(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		// OpenAI and the compatible cloud vendors.
		{"openai context length", `{"error":{"type":"invalid_request_error","code":"context_length_exceeded"}}`, ErrCodeContextLengthExceeded},
		{"openai model not found", `{"error":{"type":"invalid_request_error","code":"model_not_found"}}`, ErrCodeModelNotFound},
		{"openai code we don't track", `{"error":{"type":"invalid_request_error","code":"invalid_api_key"}}`, ""},
		{"openai null code", `{"error":{"type":"invalid_request_error","code":null}}`, ""},

		// llama.cpp / Ollama: the reason is in error.type, and code is numeric.
		{"llama.cpp context size", `{"error":{"code":500,"message":"context size exceeded","type":"exceed_context_size_error"}}`, ErrCodeContextLengthExceeded},
		{"llama.cpp other type", `{"error":{"code":500,"message":"boom","type":"server_error"}}`, ""},

		// Google / Vertex AI: numeric code, reason in a status enum.
		{"google not found", `{"error":{"code":404,"message":"Publisher Model ` + "`" + `google/gemini-9` + "`" + ` was not found","status":"NOT_FOUND"}}`, ErrCodeModelNotFound},
		{"google token count over the window", `{"error":{"code":400,"message":"The input token count (1200000) exceeds the maximum number of tokens allowed (1048576).","status":"INVALID_ARGUMENT"}}`, ErrCodeContextLengthExceeded},
		{"google invalid argument about something else", `{"error":{"code":400,"message":"Unable to submit request because tool_config is invalid.","status":"INVALID_ARGUMENT"}}`, ""},
		{"google permission denied", `{"error":{"code":403,"message":"Permission denied on resource project acme.","status":"PERMISSION_DENIED"}}`, ""},

		// Nothing recognizable.
		{"empty body", ``, ""},
		{"html error page", `<html>502 Bad Gateway</html>`, ""},
		{"json without an error object", `{"detail":"nope"}`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyErrorBody([]byte(c.body)); got != c.want {
				t.Errorf("classifyErrorBody() = %q, want %q", got, c.want)
			}
		})
	}
}

// TestHTTPErrorRetryability is the other half of what httpError decides: a
// classified code makes an error fallback-worthy, but the status alone still
// decides whether retrying the SAME target is worth it.
func TestHTTPErrorRetryability(t *testing.T) {
	notFound := httpError("vx", 404, []byte(`{"error":{"code":404,"message":"nope","status":"NOT_FOUND"}}`))
	if IsRetryable(notFound) {
		t.Error("a 404 must not be retryable against the same target")
	}
	if !IsFallbackWorthy(notFound) {
		t.Error("a Vertex NOT_FOUND must be fallback-worthy, so the router tries the next tier")
	}
	if !IsRetryable(httpError("vx", 503, nil)) {
		t.Error("a 503 must stay retryable")
	}
}
