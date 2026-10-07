package providers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// TestParseRetryAfter covers both forms RFC 9110 §10.2.3 allows (seconds,
// HTTP-date), confirmed against a real upstream's actual response (Muse
// Spark sends "retry-after: 60" on its 503) — not invented from the spec
// alone.
func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		value   string
		wantOK  bool
		wantDur time.Duration
	}{
		{"seconds form, confirmed live against Muse Spark", "60", true, 60 * time.Second},
		{"zero seconds", "0", true, 0},
		{"HTTP-date form, 30s in the future", now.Add(30 * time.Second).Format(http.TimeFormat), true, 30 * time.Second},
		{"HTTP-date form, in the past", now.Add(-30 * time.Second).Format(http.TimeFormat), true, 0},
		{"empty", "", false, 0},
		{"garbage", "not-a-duration", false, 0},
		{"negative seconds", "-5", false, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := parseRetryAfter(c.value, now)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v", ok, c.wantOK)
			}
			if ok && got != c.wantDur {
				t.Errorf("duration = %v, want %v", got, c.wantDur)
			}
		})
	}
}

// TestHTTPErrorPopulatesRetryAfter is the Retry-After plumbing this gateway
// didn't have at all before: httpError now reads it from the response
// header it's handed, so a target the vendor explicitly says "retry after
// 60s" on doesn't rely on a guessed backoff.
func TestHTTPErrorPopulatesRetryAfter(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", "60")
	pe, ok := httpError("muse", 503, []byte(`{"error":{"code":"service_overloaded"}}`), h).(*Error)
	if !ok {
		t.Fatal("httpError must return *Error")
	}
	if pe.RetryAfter == nil || *pe.RetryAfter != 60*time.Second {
		t.Errorf("RetryAfter = %v, want 60s", pe.RetryAfter)
	}
}

// TestHTTPErrorNoRetryAfterHeaderLeavesItNil proves the zero value and
// "absent" stay distinguishable: a nil header (today's every other call
// site before this change) and a header with no Retry-After must both
// leave RetryAfter nil, not a fabricated 0s.
func TestHTTPErrorNoRetryAfterHeaderLeavesItNil(t *testing.T) {
	pe, ok := httpError("muse", 503, []byte(`{}`), nil).(*Error)
	if !ok {
		t.Fatal("httpError must return *Error")
	}
	if pe.RetryAfter != nil {
		t.Errorf("RetryAfter = %v, want nil (no header offered)", pe.RetryAfter)
	}

	pe, ok = httpError("muse", 503, []byte(`{}`), http.Header{}).(*Error)
	if !ok {
		t.Fatal("httpError must return *Error")
	}
	if pe.RetryAfter != nil {
		t.Errorf("RetryAfter = %v, want nil (header present but empty)", pe.RetryAfter)
	}
}

func TestIsFallbackWorthyRetryable(t *testing.T) {
	err := &Error{Status: 503, Retryable: true}
	if !IsFallbackWorthy(err) {
		t.Error("a retryable error must be fallback-worthy")
	}
}

func TestIsFallbackWorthyKnownCode(t *testing.T) {
	for _, code := range []string{ErrCodeContextLengthExceeded, ErrCodeModelNotFound, ErrCodeMultimodalNotSupported, ErrCodeReasoningEffortUnsupported} {
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
		{"openai reasoning_effort rejected", `{"error":{"type":"invalid_request_error","code":null,"param":"reasoning_effort","message":"Function tools with reasoning_effort are not supported for gpt-6-astra in /v1/chat/completions."}}`, ErrCodeReasoningEffortUnsupported},
		{"openai unrelated param rejected", `{"error":{"type":"invalid_request_error","code":null,"param":"top_p","message":"Unknown parameter"}}`, ""},
		{"openai reasoning_effort itself invalid (client's own mistake, not a model limitation)", `{"error":{"type":"invalid_request_error","code":"invalid_value","param":"reasoning_effort","message":"Invalid value: 'extreme'. Supported values are: 'low', 'medium', 'high'."}}`, ""},

		// llama.cpp / Ollama: the reason is in error.type, and code is numeric.
		{"llama.cpp context size", `{"error":{"code":500,"message":"context size exceeded","type":"exceed_context_size_error"}}`, ErrCodeContextLengthExceeded},
		{"llama.cpp other type", `{"error":{"code":500,"message":"boom","type":"server_error"}}`, ""},

		// Google / Vertex AI: numeric code, reason in a status enum.
		{"google not found", `{"error":{"code":404,"message":"Publisher Model ` + "`" + `google/gemini-9` + "`" + ` was not found","status":"NOT_FOUND"}}`, ErrCodeModelNotFound},
		{"google token count over the window", `{"error":{"code":400,"message":"The input token count (1200000) exceeds the maximum number of tokens allowed (1048576).","status":"INVALID_ARGUMENT"}}`, ErrCodeContextLengthExceeded},
		{"google invalid argument about something else", `{"error":{"code":400,"message":"Unable to submit request because tool_config is invalid.","status":"INVALID_ARGUMENT"}}`, ""},
		{"google permission denied", `{"error":{"code":403,"message":"Permission denied on resource project acme.","status":"PERMISSION_DENIED"}}`, ErrCodeProviderAuth},
		{"google unauthenticated", `{"error":{"code":401,"message":"Request had invalid authentication credentials.","status":"UNAUTHENTICATED"}}`, ErrCodeProviderAuth},
		{"google failed precondition", `{"error":{"code":400,"message":"Project is not allowed to use this service.","status":"FAILED_PRECONDITION"}}`, ErrCodeProviderAuth},
		{"google billing disabled", `{"error":{"code":403,"message":"This API method requires billing to be enabled.","status":"PERMISSION_DENIED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"BILLING_DISABLED"}]}}`, ErrCodeProviderAuth},

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

// TestTransportErrorCancellation proves a canceled caller context is
// classified as non-retryable/non-fallback-worthy (Routing/fallback I1
// fix) — in this codebase that's almost always the original client
// disconnecting, which another tier can't fix, so a full tier-walk for it
// would just waste real upstream calls nobody is waiting for. Any other
// transport failure (a dial error, a reset connection) stays retryable,
// unchanged.
func TestTransportErrorCancellation(t *testing.T) {
	canceled := transportError(context.Canceled)
	if IsRetryable(canceled) {
		t.Error("context.Canceled must not be retryable")
	}
	if IsFallbackWorthy(canceled) {
		t.Error("context.Canceled must not be fallback-worthy")
	}

	// http.Client.Do wraps a canceled-context failure inside *url.Error,
	// not just the bare sentinel — errors.Is must still see through it.
	wrapped := transportError(fmt.Errorf("Post \"http://x\": %w", context.Canceled))
	if IsRetryable(wrapped) {
		t.Error("a wrapped context.Canceled must not be retryable")
	}

	plain := transportError(errors.New("dial tcp: connection refused"))
	if !IsRetryable(plain) {
		t.Error("a genuine transport failure must stay retryable")
	}
}

// TestHTTPErrorRetryability is the other half of what httpError decides: a
// classified code makes an error fallback-worthy, but the status alone still
// decides whether retrying the SAME target is worth it.
func TestHTTPErrorRetryability(t *testing.T) {
	notFound := httpError("vx", 404, []byte(`{"error":{"code":404,"message":"nope","status":"NOT_FOUND"}}`), nil)
	if IsRetryable(notFound) {
		t.Error("a 404 must not be retryable against the same target")
	}
	if !IsFallbackWorthy(notFound) {
		t.Error("a Vertex NOT_FOUND must be fallback-worthy, so the router tries the next tier")
	}
	if !IsRetryable(httpError("vx", 503, nil, nil)) {
		t.Error("a 503 must stay retryable")
	}
}

// TestHTTPErrorStatusOnlyCodes covers the codes httpError derives from the
// status when the body names none: an auth refusal by any vendor, and
// Ollama's English-only "model not found".
func TestHTTPErrorStatusOnlyCodes(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"openai invalid key", 401, `{"error":{"message":"Incorrect API key provided","type":"invalid_request_error","code":"invalid_api_key"}}`, ErrCodeProviderAuth},
		{"bare 403", 403, `forbidden`, ErrCodeProviderAuth},
		{"ollama openai-compat model not found", 404, `{"error":{"message":"model \"gemma3:27b\" not found, try pulling it first","type":"api_error","param":null,"code":null}}`, ErrCodeModelNotFound},
		{"ollama native model not found", 404, `{"error":"model 'gemma3:27b' not found"}`, ErrCodeModelNotFound},
		{"a 404 about something else", 404, `404 page not found`, ""},
		{"a 400 mentioning a missing model is not a 404", 400, `{"error":{"message":"model x not found"}}`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var pe *Error
			if !errors.As(httpError("up", c.status, []byte(c.body), nil), &pe) || pe.Code != c.want {
				t.Errorf("code = %q, want %q", pe.Code, c.want)
			}
		})
	}
	if IsFallbackWorthy(httpError("up", 401, nil, nil)) {
		t.Error("an auth refusal must not be fallback-worthy by itself; that is a per-target choice")
	}
	if !IsAuthFailure(httpError("up", 403, nil, nil)) {
		t.Error("a 403 must be recognised as an auth failure")
	}
}
