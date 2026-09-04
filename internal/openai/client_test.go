package openai

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/llm"
)

func TestEncodeChatRequestNoImagesUnchanged(t *testing.T) {
	req := llm.ChatRequest{Model: "m", Messages: []llm.Message{{Role: "user", Content: "hi"}}}
	b, err := EncodeChatRequest(req, false)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	msgs := got["messages"].([]any)
	msg := msgs[0].(map[string]any)
	if content, ok := msg["content"].(string); !ok || content != "hi" {
		t.Errorf("content = %#v, want plain string \"hi\"", msg["content"])
	}
}

func TestEncodeChatRequestWithOneImage(t *testing.T) {
	req := llm.ChatRequest{Model: "m", Messages: []llm.Message{{
		Role: "user", Content: "what is this?",
		Images: []llm.Image{{URL: "data:image/png;base64,AAAA"}},
	}}}
	b, err := EncodeChatRequest(req, false)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	msg := got["messages"].([]any)[0].(map[string]any)
	parts, ok := msg["content"].([]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("content = %#v, want a 2-part array", msg["content"])
	}
	textPart := parts[0].(map[string]any)
	if textPart["type"] != "text" || textPart["text"] != "what is this?" {
		t.Errorf("text part = %#v", textPart)
	}
	imgPart := parts[1].(map[string]any)
	if imgPart["type"] != "image_url" {
		t.Errorf("image part type = %#v", imgPart["type"])
	}
	imgURL := imgPart["image_url"].(map[string]any)
	if imgURL["url"] != "data:image/png;base64,AAAA" {
		t.Errorf("image url = %#v", imgURL["url"])
	}
	if _, hasDetail := imgURL["detail"]; hasDetail {
		t.Error("detail should be omitted when unset")
	}
}

func TestEncodeChatRequestImageWithDetail(t *testing.T) {
	req := llm.ChatRequest{Model: "m", Messages: []llm.Message{{
		Role: "user", Images: []llm.Image{{URL: "https://x/a.png", Detail: "high"}},
	}}}
	b, err := EncodeChatRequest(req, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"detail":"high"`) {
		t.Errorf("expected detail to be included, got %s", b)
	}
}

func TestEncodeChatRequestEmptyContentNoImagesOmitsContentKey(t *testing.T) {
	// A tool-calls-only assistant message: Content == "", no Images.
	// Today's plain `Content string `json:"content,omitempty"`` field
	// omits the key entirely in this case — this must not regress once
	// Content becomes `any` on the wire type (see toOpenAIOutMessage's
	// comment on why a naive assignment would break this).
	req := llm.ChatRequest{Model: "m", Messages: []llm.Message{{
		Role:      "assistant",
		ToolCalls: []llm.ToolCall{{ID: "1", Type: "function", Function: llm.FunctionCall{Name: "f", Arguments: "{}"}}},
	}}}
	b, err := EncodeChatRequest(req, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"content"`) {
		t.Errorf("content key must be omitted entirely for empty content with no images, got: %s", b)
	}
}

func TestEncodeChatRequestImageOnlyOmitsTextPart(t *testing.T) {
	req := llm.ChatRequest{Model: "m", Messages: []llm.Message{{
		Role: "user", Images: []llm.Image{{URL: "https://x/a.png"}},
	}}}
	b, err := EncodeChatRequest(req, false)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	parts := got["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if len(parts) != 1 {
		t.Fatalf("want exactly 1 part (image only, no text part), got %d: %#v", len(parts), parts)
	}
}
