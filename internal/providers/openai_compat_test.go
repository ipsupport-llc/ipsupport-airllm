package providers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/llm"
)

// sseServer serves the given raw "data: ..." lines (already SSE-formatted,
// including the trailing "data: [DONE]" when the test wants one) as a
// text/event-stream response.
func sseServer(lines ...string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, l := range lines {
			fmt.Fprintf(w, "data: %s\n\n", l)
		}
	}))
}

func collectChunks(t *testing.T, ts *httptest.Server) []llm.StreamChunk {
	t.Helper()
	p := NewOpenAICompat("up", "openai", ts.URL, "sk-test")
	var got []llm.StreamChunk
	err := p.ChatStream(context.Background(), llm.ChatRequest{Model: "m", Messages: []llm.Message{{Role: "user", Content: "hi"}}},
		func(c llm.StreamChunk) error {
			got = append(got, c)
			return nil
		})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	return got
}

func TestChatStreamSynthesizesFinishReasonBeforeUsage(t *testing.T) {
	// Groq-shaped quirk: a whole tool call, then straight to a usage chunk,
	// finish_reason never populated anywhere (see the v0.1.12 incident).
	ts := sseServer(
		`{"choices":[{"delta":{"role":"assistant"},"finish_reason":null}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"file","arguments":"{\"a\":1}"}}]},"finish_reason":null}]}`,
		`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
		"[DONE]",
	)
	defer ts.Close()

	got := collectChunks(t, ts)
	if len(got) != 4 {
		t.Fatalf("want 4 chunks (role, tool_calls, synthesized finish, usage), got %d: %+v", len(got), got)
	}
	finish := got[2]
	if finish.FinishReason != "tool_calls" {
		t.Errorf("synthesized finish_reason = %q, want tool_calls", finish.FinishReason)
	}
	if got[3].Usage == nil {
		t.Errorf("usage chunk lost or reordered: %+v", got[3])
	}
}

func TestChatStreamSynthesizesStopForPlainText(t *testing.T) {
	ts := sseServer(
		`{"choices":[{"delta":{"role":"assistant"},"finish_reason":null}]}`,
		`{"choices":[{"delta":{"content":"hi"},"finish_reason":null}]}`,
		`{"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`,
		"[DONE]",
	)
	defer ts.Close()

	got := collectChunks(t, ts)
	if len(got) != 4 {
		t.Fatalf("want 4 chunks, got %d: %+v", len(got), got)
	}
	if got[2].FinishReason != "stop" {
		t.Errorf("synthesized finish_reason = %q, want stop", got[2].FinishReason)
	}
}

func TestChatStreamNoSynthesisWhenUpstreamSendsFinish(t *testing.T) {
	ts := sseServer(
		`{"choices":[{"delta":{"content":"hi"},"finish_reason":null}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`,
		"[DONE]",
	)
	defer ts.Close()

	got := collectChunks(t, ts)
	if len(got) != 3 {
		t.Fatalf("well-behaved upstream must not get an extra synthesized chunk, got %d: %+v", len(got), got)
	}
	if got[1].FinishReason != "stop" {
		t.Errorf("finish_reason = %q, want stop", got[1].FinishReason)
	}
}

func TestChatStreamSplitsBundledFinishAndUsage(t *testing.T) {
	// Real Groq shape (captured live, see the v0.1.13 incident): the
	// finish_reason chunk and the terminal usage chunk are the SAME JSON
	// object, not separate ones like OpenAI/xAI send.
	ts := sseServer(
		`{"choices":[{"delta":{"role":"assistant"},"finish_reason":null}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"file","arguments":"{\"a\":1}"}}]},"finish_reason":null}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":291,"completion_tokens":246,"total_tokens":537}}`,
		"[DONE]",
	)
	defer ts.Close()

	got := collectChunks(t, ts)
	if len(got) != 4 {
		t.Fatalf("want 4 chunks (role, tool_calls, finish, usage — split apart), got %d: %+v", len(got), got)
	}
	finish, usage := got[2], got[3]
	if finish.FinishReason != "tool_calls" {
		t.Errorf("finish chunk lost: %+v", finish)
	}
	if finish.Usage != nil {
		t.Errorf("finish chunk must not carry usage once split: %+v", finish)
	}
	if usage.Usage == nil || usage.FinishReason != "" {
		t.Errorf("usage chunk must be usage-only after the split: %+v", usage)
	}
}

func TestChatStreamSynthesizesAtStreamEndWithNoUsageChunk(t *testing.T) {
	// No usage chunk at all before [DONE] — still must not leave the client
	// without a finish signal.
	ts := sseServer(
		`{"choices":[{"delta":{"content":"hi"},"finish_reason":null}]}`,
		"[DONE]",
	)
	defer ts.Close()

	got := collectChunks(t, ts)
	if len(got) != 2 {
		t.Fatalf("want 2 chunks (content, synthesized finish), got %d: %+v", len(got), got)
	}
	if got[1].FinishReason != "stop" {
		t.Errorf("finish_reason = %q, want stop", got[1].FinishReason)
	}
}

func TestHTTPErrorParsesOpenAIStyleContextLengthExceeded(t *testing.T) {
	body := []byte(`{"error":{"message":"This model's maximum context length is 8192 tokens.","type":"invalid_request_error","code":"context_length_exceeded"}}`)
	err := httpError("openai", 400, body)
	pe, ok := err.(*Error)
	if !ok {
		t.Fatalf("want *Error, got %T", err)
	}
	if pe.Code != ErrCodeContextLengthExceeded {
		t.Errorf("Code = %q, want %q", pe.Code, ErrCodeContextLengthExceeded)
	}
	if pe.Retryable {
		t.Error("a 400 must still be Retryable=false — Code is additive, not a replacement")
	}
}

func TestHTTPErrorParsesOpenAIStyleModelNotFound(t *testing.T) {
	body := []byte(`{"error":{"message":"The model does not exist","type":"invalid_request_error","code":"model_not_found"}}`)
	err := httpError("groq", 404, body)
	pe := err.(*Error)
	if pe.Code != ErrCodeModelNotFound {
		t.Errorf("Code = %q, want %q", pe.Code, ErrCodeModelNotFound)
	}
}

func TestHTTPErrorParsesLlamaCppStyleContextSizeExceeded(t *testing.T) {
	body := []byte(`{"error":{"code":400,"message":"the request exceeds the available context size, try increasing it","type":"exceed_context_size_error","n_prompt_tokens":94520,"n_ctx":8192}}`)
	err := httpError("ollama-local", 400, body)
	pe := err.(*Error)
	if pe.Code != ErrCodeContextLengthExceeded {
		t.Errorf("Code = %q, want %q", pe.Code, ErrCodeContextLengthExceeded)
	}
}

func TestHTTPErrorParsesOllamaMultimodalRejection(t *testing.T) {
	// Real body captured live from Ollama's OpenAI-compat shim rejecting an
	// image sent to a non-vision model: double-nested, the outer envelope's
	// own code is null and its message is a JSON-encoded string holding the
	// real error.
	body := []byte(`{"error":{"message":"{\"error\":{\"code\":400,\"message\":\"Multimodal data provided, but model does not support multimodal requests.\",\"type\":\"invalid_request_error\"}}","type":"invalid_request_error","param":null,"code":null}}`)
	err := httpError("ollama-local", 400, body)
	pe := err.(*Error)
	if pe.Code != ErrCodeMultimodalNotSupported {
		t.Errorf("Code = %q, want %q", pe.Code, ErrCodeMultimodalNotSupported)
	}
	if pe.Retryable {
		t.Error("a 400 must still be Retryable=false — Code is additive, not a replacement")
	}
}

func TestHTTPErrorUnrecognizedBodyLeavesCodeEmpty(t *testing.T) {
	cases := [][]byte{
		[]byte(`{"error":{"message":"bad request","type":"invalid_request_error","code":"something_else"}}`),
		[]byte(`{"error":"model 'x' not found, try pulling it first"}`), // Ollama's plain-string shape, not object
		[]byte(`not even json`),
		[]byte(``),
		[]byte(`{"error":{"type":"invalid_request_error","code":null}}`), // OpenAI sends real null codes for many error kinds
		[]byte(`{"error":{"message":"model does not support tool calling","type":"invalid_request_error","code":null}}`), // similarly generic type/code, unrelated message — must not false-positive on "multimodal"
	}
	for _, body := range cases {
		err := httpError("x", 400, body)
		pe := err.(*Error)
		if pe.Code != "" {
			t.Errorf("body %q: Code = %q, want empty", body, pe.Code)
		}
	}
}

func TestHTTPErrorRetryableUnaffectedByCode(t *testing.T) {
	body := []byte(`{"error":{"message":"rate limited","type":"rate_limit_error"}}`)
	err := httpError("x", 429, body).(*Error)
	if !err.Retryable {
		t.Error("429 must remain Retryable=true regardless of Code parsing")
	}
	if err.Code != "" {
		t.Errorf("this body has no recognized code, want empty, got %q", err.Code)
	}
}
