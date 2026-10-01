package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/llm"
)

const geminiSignature = `{"google":{"thought_signature":"c2lnLUE="}}`

const (
	okChat   = `{"id":"r","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`
	okStream = "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
)

// parallelToolHistory is one step of a tool-using turn: two parallel calls,
// where only the first carries extra (as Gemini signs only the first), and
// their two results.
func parallelToolHistory(extra json.RawMessage) llm.ChatRequest {
	return llm.ChatRequest{Model: "gemini-3.5-flash-lite", Messages: []llm.Message{
		{Role: "user", Content: "weather in Paris and Tokyo?"},
		{Role: "assistant", ToolCalls: []llm.ToolCall{
			{ID: "c1", Type: "function", Function: llm.FunctionCall{Name: "get_weather", Arguments: `{"city":"Paris"}`}, ExtraContent: extra},
			{ID: "c2", Type: "function", Function: llm.FunctionCall{Name: "get_weather", Arguments: `{"city":"Tokyo"}`}},
		}},
		{Role: "tool", ToolCallID: "c1", Content: "18C"},
		{Role: "tool", ToolCallID: "c2", Content: "24C"},
	}}
}

// sentExtras returns the extra_content of each tool call in the upstream
// request's assistant message, re-marshalled compactly; "" when absent.
func sentExtras(t *testing.T, body map[string]any) []string {
	t.Helper()
	msgs, _ := body["messages"].([]any)
	if len(msgs) < 2 {
		t.Fatalf("upstream messages = %v", body["messages"])
	}
	calls, _ := msgs[1].(map[string]any)["tool_calls"].([]any)
	out := make([]string, len(calls))
	for i, c := range calls {
		if v, ok := c.(map[string]any)["extra_content"]; ok {
			b, _ := json.Marshal(v)
			out[i] = string(b)
		}
	}
	return out
}

type chatCall func(p Provider, req llm.ChatRequest) error

func viaChat(p Provider, req llm.ChatRequest) error {
	_, err := p.Chat(context.Background(), req)
	return err
}

func viaStream(p Provider, req llm.ChatRequest) error {
	return p.ChatStream(context.Background(), req, func(llm.StreamChunk) error { return nil })
}

var callModes = map[string]struct {
	call        chatCall
	contentType string
	resp        string
}{
	"chat":   {viaChat, "application/json", okChat},
	"stream": {viaStream, "text/event-stream", okStream},
}

func TestOpenAICompatDoesNotSendToolCallExtras(t *testing.T) {
	for mode, m := range callModes {
		t.Run(mode, func(t *testing.T) {
			up := newVertexUpstream(t, http.StatusOK, m.contentType, m.resp)
			p := NewOpenAICompat("up", "openai", up.URL, "sk-test")
			req := parallelToolHistory(json.RawMessage(geminiSignature))
			if err := m.call(p, req); err != nil {
				t.Fatal(err)
			}
			if got := sentExtras(t, up.body); got[0] != "" || got[1] != "" {
				t.Errorf("extra_content sent to a non-Vertex upstream: %q", got)
			}
			if got := string(req.Messages[1].ToolCalls[0].ExtraContent); got != geminiSignature {
				t.Errorf("caller's request edited: extra_content now %q; the next tier needs it", got)
			}
		})
	}
}

func TestVertexSendsTheThoughtSignatureBack(t *testing.T) {
	// Google's documented stand-in for function calls a model without
	// signatures produced: a fallback tier mid-turn, or a client that does
	// not echo extra_content.
	const dummy = `{"google":{"thought_signature":"skip_thought_signature_validator"}}`
	for name, tc := range map[string]struct {
		extra     json.RawMessage
		wantFirst string
	}{
		"its own signature, untouched": {json.RawMessage(geminiSignature), geminiSignature},
		"no signature, dummy on first": {nil, dummy},
	} {
		for mode, m := range callModes {
			t.Run(name+"/"+mode, func(t *testing.T) {
				up := newVertexUpstream(t, http.StatusOK, m.contentType, m.resp)
				p := NewVertex("vx", up.URL, stubTokenSource{token: "ya29.stub"})
				req := parallelToolHistory(tc.extra)
				if err := m.call(p, req); err != nil {
					t.Fatal(err)
				}
				got := sentExtras(t, up.body)
				if got[0] != tc.wantFirst || got[1] != "" {
					t.Errorf("extra_content sent = %q, want [%q, absent]", got, tc.wantFirst)
				}
				if got := string(req.Messages[1].ToolCalls[0].ExtraContent); got != string(tc.extra) {
					t.Errorf("caller's request edited: extra_content now %q", got)
				}
			})
		}
	}
}
