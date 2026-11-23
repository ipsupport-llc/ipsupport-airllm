package providers

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/llm"
)

// TestDecodeSSEStreamErrorEnvelopeIsAnError: an upstream that reports a
// failure inside a 2xx stream (vLLM, llama.cpp, OpenRouter) must end the
// stream with a provider error, not an empty chunk followed by a synthesized
// "stop" that dresses the failure up as a complete answer.
func TestDecodeSSEStreamErrorEnvelopeIsAnError(t *testing.T) {
	cases := []struct {
		name       string
		data       string
		wantStatus int
		retryable  bool
	}{
		{"numeric 5xx code", `{"error":{"message":"CUDA out of memory","type":"server_error","code":500}}`, 500, true},
		{"numeric 4xx code", `{"error":{"object":"error","message":"bad input","type":"BadRequestError","code":400}}`, 400, false},
		{"string code", `{"id":"x","object":"chat.completion.chunk","error":{"code":"server_error","message":"provider died"},"choices":[{"delta":{"content":""},"finish_reason":"error"}]}`, http.StatusBadGateway, true},
		{"bare string", `{"error":"overloaded"}`, http.StatusBadGateway, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n" +
				"data: " + c.data + "\n\n" +
				"data: [DONE]\n\n"
			var got []llm.StreamChunk
			err := decodeSSEStream(strings.NewReader(body), streamDecodeOptions{provider: "up"}, func(ch llm.StreamChunk) error {
				got = append(got, ch)
				return nil
			})
			var pe *Error
			if !errors.As(err, &pe) {
				t.Fatalf("want *Error, got %v", err)
			}
			if pe.Status != c.wantStatus || pe.Retryable != c.retryable {
				t.Errorf("status=%d retryable=%v, want %d %v", pe.Status, pe.Retryable, c.wantStatus, c.retryable)
			}
			if pe.VendorMessage == "" {
				t.Errorf("vendor message not extracted from %s", c.data)
			}
			if len(got) != 1 || got[0].Content != "partial" || got[0].FinishReason != "" {
				t.Errorf("want only the partial chunk before the error, got %+v", got)
			}
		})
	}
}

// TestDecodeSSEStreamNullErrorIsNotAnError guards the detection: a chunk
// carrying "error": null is an ordinary chunk.
func TestDecodeSSEStreamNullErrorIsNotAnError(t *testing.T) {
	body := "data: {\"error\":null,\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"
	got := decodeAll(t, body, streamDecodeOptions{})
	if len(got) == 0 || got[0].Content != "hi" {
		t.Fatalf("want the content chunk, got %+v", got)
	}
}

func TestHTTPErrorKeepsVendorMessageApart(t *testing.T) {
	err := httpError("vertex-prod", 400, []byte(`{"error":{"code":400,"message":"model projects/acme-123/locations/x not found","status":"INVALID_ARGUMENT"}}`), nil)
	var pe *Error
	if !errors.As(err, &pe) {
		t.Fatal("want *Error")
	}
	if pe.VendorMessage != "model projects/acme-123/locations/x not found" {
		t.Errorf("VendorMessage = %q", pe.VendorMessage)
	}
	if !strings.Contains(pe.Message, "vertex-prod") {
		t.Errorf("Message must keep the full detail for logs, got %q", pe.Message)
	}
}
