package providers

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/llm"
)

// slowServer answers body after delay, or gives up when the client does.
func slowServer(t *testing.T, delay time.Duration, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
			return
		case <-time.After(delay):
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

const (
	slowChatBody       = `{"id":"c","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
	slowEmbeddingsBody = `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.1,0.2]}],"model":"m","usage":{"prompt_tokens":1,"total_tokens":1}}`
)

var chatIn = llm.ChatRequest{Model: "m", Messages: []llm.Message{{Role: "user", Content: "hi"}}}

// wantCeilingTimeout checks err is the ceiling's own timeout error.
func wantCeilingTimeout(t *testing.T, err error) {
	t.Helper()
	var pe *Error
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v, want a *providers.Error", err)
	}
	if pe.Code != ErrCodeUpstreamTimeout || pe.Status != http.StatusGatewayTimeout || !pe.Retryable {
		t.Errorf("err = %+v, want code %s, status 504, retryable", pe, ErrCodeUpstreamTimeout)
	}
}

func TestCallBudgetReplacesTheChatCeiling(t *testing.T) {
	up := slowServer(t, 150*time.Millisecond, slowChatBody)
	p := NewOpenAICompat("up", "openai", up.URL, "")
	p.SetChatTimeout(50 * time.Millisecond)

	ctx := WithCallBudget(context.Background(), time.Second)
	resp, err := p.Chat(ctx, chatIn)
	if err != nil {
		t.Fatalf("Chat with a 1s budget over a 50ms ceiling: %v", err)
	}
	if len(resp.Choices) != 1 {
		t.Errorf("choices = %d, want 1", len(resp.Choices))
	}
}

func TestChatCeilingWithoutBudgetIsATimeout(t *testing.T) {
	up := slowServer(t, 150*time.Millisecond, slowChatBody)
	p := NewOpenAICompat("up", "openai", up.URL, "")
	p.SetChatTimeout(50 * time.Millisecond)

	_, err := p.Chat(context.Background(), chatIn)
	wantCeilingTimeout(t, err)

	// A zero budget is no budget: the ceiling still applies.
	_, err = p.Chat(WithCallBudget(context.Background(), 0), chatIn)
	wantCeilingTimeout(t, err)
}

func TestChatCallerCancelIsNotACeilingTimeout(t *testing.T) {
	up := slowServer(t, time.Second, slowChatBody)
	p := NewOpenAICompat("up", "openai", up.URL, "")
	p.SetChatTimeout(time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := p.Chat(ctx, chatIn)
	var pe *Error
	if errors.As(err, &pe) && pe.Code == ErrCodeUpstreamTimeout {
		t.Fatalf("err = %v: the caller's own deadline must not read as the ceiling's", err)
	}
}

func TestCallBudgetReplacesTheEmbeddingsCeiling(t *testing.T) {
	up := slowServer(t, 150*time.Millisecond, slowEmbeddingsBody)
	p := NewOpenAICompat("up", "openai", up.URL, "")
	p.SetChatTimeout(50 * time.Millisecond)
	in := llm.EmbeddingsRequest{Model: "m", Input: []string{"hi"}}

	if _, err := p.Embeddings(WithCallBudget(context.Background(), time.Second), in); err != nil {
		t.Fatalf("Embeddings with a 1s budget over a 50ms ceiling: %v", err)
	}
	_, err := p.Embeddings(context.Background(), in)
	wantCeilingTimeout(t, err)
}

func TestCallBudgetReplacesTheVertexCeiling(t *testing.T) {
	up := slowServer(t, 150*time.Millisecond, slowChatBody)
	p := NewVertex("vx", up.URL, stubTokenSource{token: "t"})
	p.SetChatTimeout(50 * time.Millisecond)

	if _, err := p.Chat(WithCallBudget(context.Background(), time.Second), chatIn); err != nil {
		t.Fatalf("Vertex Chat with a 1s budget over a 50ms ceiling: %v", err)
	}
	_, err := p.Chat(context.Background(), chatIn)
	wantCeilingTimeout(t, err)
}

func TestChatCeilingDuringBodyReadIsATimeout(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id":"c","choices":[`)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(up.Close)
	p := NewOpenAICompat("up", "openai", up.URL, "")
	p.SetChatTimeout(50 * time.Millisecond)

	_, err := p.Chat(context.Background(), chatIn)
	wantCeilingTimeout(t, err)
}

// TestCeilingErrorOnlyReplacesTheDeadline: once the ceiling has passed,
// only the deadline itself becomes upstream_timeout; an upstream answer or
// an unrelated failure that came back at that moment is left as it is.
func TestCeilingErrorOnlyReplacesTheDeadline(t *testing.T) {
	parent := context.Background()
	cctx, cancel := context.WithTimeout(parent, time.Nanosecond)
	defer cancel()
	<-cctx.Done()

	answer := httpError("up", http.StatusServiceUnavailable, []byte(`{"error":{"message":"overloaded"}}`), nil)
	if got := ceilingError(parent, cctx, "up", time.Second, answer); got != answer {
		t.Errorf("an upstream 503 was replaced: %v", got)
	}
	other := errors.New("invalid character in response")
	if got := ceilingError(parent, cctx, "up", time.Second, other); got != other {
		t.Errorf("an unrelated error was replaced: %v", got)
	}
	transport := transportError(&url.Error{Op: "Post", URL: "http://x", Err: context.DeadlineExceeded})
	wantCeilingTimeout(t, ceilingError(parent, cctx, "up", time.Second, transport))
}

func TestIsUpstreamAnswer(t *testing.T) {
	parent := context.Background()
	cctx, cancel := context.WithTimeout(parent, time.Nanosecond)
	defer cancel()
	<-cctx.Done()
	transport := transportError(&url.Error{Op: "Post", URL: "http://x", Err: context.DeadlineExceeded})

	for name, c := range map[string]struct {
		err  error
		want bool
	}{
		"503 answer":         {httpError("up", http.StatusServiceUnavailable, []byte(`{}`), nil), true},
		"400 answer":         {httpError("up", http.StatusBadRequest, []byte(`{}`), nil), true},
		"transport deadline": {transport, false},
		"transport cancel":   {transportError(context.Canceled), false},
		"ceiling timeout":    {ceilingError(parent, cctx, "up", time.Second, transport), false},
		"plain error":        {errors.New("decode"), false},
	} {
		if got := IsUpstreamAnswer(c.err); got != c.want {
			t.Errorf("%s: IsUpstreamAnswer = %v, want %v", name, got, c.want)
		}
	}
}
