package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/llm"
)

// TestEmbeddingsHitsEmbeddingsPath proves OpenAICompat.Embeddings posts to
// base_url+"/embeddings" (not /chat/completions) and decodes the response
// in request order.
func TestEmbeddingsHitsEmbeddingsPath(t *testing.T) {
	var gotPath string
	var gotBody struct {
		Model string   `json:"model"`
		Input []string `json:"input"`
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"model":"m","data":[{"embedding":[0.1,0.2],"index":0}],"usage":{"prompt_tokens":3}}`))
	}))
	defer ts.Close()

	p := NewOpenAICompat("up", "openai", ts.URL, "sk-test")
	resp, err := p.Embeddings(context.Background(), llm.EmbeddingsRequest{Model: "m", Input: []string{"hi"}})
	if err != nil {
		t.Fatalf("Embeddings: %v", err)
	}
	if gotPath != "/embeddings" {
		t.Errorf("upstream path = %q, want /embeddings", gotPath)
	}
	if gotBody.Model != "m" || len(gotBody.Input) != 1 || gotBody.Input[0] != "hi" {
		t.Errorf("upstream body = %+v", gotBody)
	}
	if len(resp.Data) != 1 || resp.Data[0][0] != 0.1 || resp.PromptTokens != 3 {
		t.Errorf("resp = %+v", resp)
	}
}

// TestEmbeddingsRejectsIncompleteResponse proves a response whose data
// doesn't fully cover the request's inputs is a retryable failure, not a
// success with missing or null vectors. Covers both a short count and
// indices that leave a hole.
func TestEmbeddingsRejectsIncompleteResponse(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"fewer entries than inputs", `{"model":"m","data":[{"embedding":[1],"index":0}],"usage":{"prompt_tokens":1}}`},
		{"indices leave a hole", `{"model":"m","data":[{"embedding":[1],"index":0},{"embedding":[2],"index":2}],"usage":{"prompt_tokens":2}}`},
		{"count matches but a duplicate index leaves a slot empty", `{"model":"m","data":[{"embedding":[1],"index":0},{"embedding":[9],"index":0},{"embedding":[2],"index":1}],"usage":{"prompt_tokens":3}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(tc.body))
			}))
			defer ts.Close()

			p := NewOpenAICompat("up", "openai", ts.URL, "sk-test")
			_, err := p.Embeddings(context.Background(), llm.EmbeddingsRequest{Model: "m", Input: []string{"a", "b", "c"}})
			if err == nil {
				t.Fatal("expected an error for an incomplete response, got none")
			}
			if !IsRetryable(err) {
				t.Errorf("expected a retryable error so failover can run, got %v", err)
			}
		})
	}
}

// TestEmbeddingsForwardsAuthAndForwardedHeaders proves Embeddings sets its
// own Authorization and also attaches an opted-in forwarded header,
// mirroring Chat's behavior on the same upstream call.
func TestEmbeddingsForwardsAuthAndForwardedHeaders(t *testing.T) {
	var gotAuth, gotForwarded string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotForwarded = r.Header.Get("X-Request-Id")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"model":"m","data":[{"embedding":[0.1],"index":0}],"usage":{"prompt_tokens":1}}`))
	}))
	defer ts.Close()

	p := NewOpenAICompat("up", "openai", ts.URL, "sk-real")
	ctx := WithForwardedHeaders(context.Background(), http.Header{"X-Request-Id": []string{"req-9"}})
	if _, err := p.Embeddings(ctx, llm.EmbeddingsRequest{Model: "m", Input: []string{"hi"}}); err != nil {
		t.Fatalf("Embeddings: %v", err)
	}
	if gotAuth != "Bearer sk-real" {
		t.Errorf("Authorization = %q, want Bearer sk-real", gotAuth)
	}
	if gotForwarded != "req-9" {
		t.Errorf("X-Request-Id = %q, want req-9", gotForwarded)
	}
}

// TestEmbeddingsRespectsConfiguredTimeout mirrors
// TestChatRespectsConfiguredTimeout for the Embeddings call.
func TestEmbeddingsRespectsConfiguredTimeout(t *testing.T) {
	block := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	defer ts.Close()
	defer close(block)

	p := NewOpenAICompat("up", "openai", ts.URL, "sk-test")
	p.SetChatTimeout(50 * time.Millisecond)

	start := time.Now()
	_, err := p.Embeddings(context.Background(), llm.EmbeddingsRequest{Model: "m", Input: []string{"hi"}})
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Embeddings took %v, want it bounded by the 50ms configured timeout", elapsed)
	}
	if err == nil {
		t.Fatal("expected a timeout error")
	}
}
