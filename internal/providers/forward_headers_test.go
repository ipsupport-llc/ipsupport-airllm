package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/llm"
)

// TestForwardedHeadersReachUpstream proves a header attached via
// WithForwardedHeaders is set on the outbound request to an
// OpenAI-compatible upstream.
func TestForwardedHeadersReachUpstream(t *testing.T) {
	var gotHeader string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("X-Request-Id")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hi"}}]}`))
	}))
	defer ts.Close()

	p := NewOpenAICompat("up", "openai", ts.URL, "sk-test")
	ctx := WithForwardedHeaders(context.Background(), http.Header{"X-Request-Id": []string{"abc-123"}})
	if _, err := p.Chat(ctx, llm.ChatRequest{Model: "m", Messages: []llm.Message{{Role: "user", Content: "hi"}}}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if gotHeader != "abc-123" {
		t.Errorf("upstream saw X-Request-Id = %q, want abc-123", gotHeader)
	}
}

// TestForwardedHeadersCannotOverrideGatewayHeaders proves a forwarded
// header can never clobber Content-Type/Authorization/Accept, which the
// gateway sets for its own reasons (upstream protocol, its own credential).
func TestForwardedHeadersCannotOverrideGatewayHeaders(t *testing.T) {
	var gotContentType, gotAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hi"}}]}`))
	}))
	defer ts.Close()

	p := NewOpenAICompat("up", "openai", ts.URL, "sk-real")
	ctx := WithForwardedHeaders(context.Background(), http.Header{
		"Content-Type":  []string{"text/plain"},
		"Authorization": []string{"Bearer evil"},
	})
	if _, err := p.Chat(ctx, llm.ChatRequest{Model: "m", Messages: []llm.Message{{Role: "user", Content: "hi"}}}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json (forwarding must not override it)", gotContentType)
	}
	if gotAuth != "Bearer sk-real" {
		t.Errorf("Authorization = %q, want Bearer sk-real (forwarding must not override it)", gotAuth)
	}
}

// TestWithForwardedHeadersEmptyIsNoop proves an empty header value leaves
// ctx unchanged, matching the withClientSession convention.
func TestWithForwardedHeadersEmptyIsNoop(t *testing.T) {
	ctx := context.Background()
	if got := WithForwardedHeaders(ctx, nil); got != ctx {
		t.Error("WithForwardedHeaders(ctx, nil) should return ctx unchanged")
	}
	if got := WithForwardedHeaders(ctx, http.Header{}); got != ctx {
		t.Error("WithForwardedHeaders(ctx, empty) should return ctx unchanged")
	}
}
