package openai

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/llm"
)

func TestDecodeChatRequestExtractsExtras(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}],` +
		`"temperature":0.5,"top_p":0.9,"stop":["a","b"],` +
		`"response_format":{"type":"json_object"},"max_completion_tokens":128}`
	req, err := DecodeChatRequest(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if req.Temperature == nil || *req.Temperature != 0.5 {
		t.Errorf("owned field lost: %+v", req.Temperature)
	}
	want := map[string]string{
		"top_p":                 `0.9`,
		"stop":                  `["a","b"]`,
		"response_format":       `{"type":"json_object"}`,
		"max_completion_tokens": `128`,
	}
	if len(req.Extra) != len(want) {
		t.Fatalf("extra keys = %v", req.Extra)
	}
	for k, v := range want {
		if string(req.Extra[k]) != v {
			t.Errorf("extra[%s] = %s, want %s", k, req.Extra[k], v)
		}
	}
}

func TestDecodeChatRequestNoExtras(t *testing.T) {
	req, err := DecodeChatRequest(strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if req.Extra != nil {
		t.Errorf("want nil Extra, got %v", req.Extra)
	}
}

// TestDecodeChatRequestStreamOptions is the Protocol-translation Minor fix:
// stream_options was listed as an "owned" key (so it never leaked into
// Extra) but chatRequestWire had no field to actually capture it — the
// client's include_usage preference was silently discarded. This asserts
// it's now threaded through to IncludeStreamUsage, and defaults to false
// when the client omits stream_options entirely (matching OpenAI's own
// documented default of no usage chunk unless explicitly requested).
func TestDecodeChatRequestStreamOptions(t *testing.T) {
	req, err := DecodeChatRequest(strings.NewReader(
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"stream_options":{"include_usage":true}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !req.IncludeStreamUsage {
		t.Error("IncludeStreamUsage = false, want true")
	}
	if req.Extra != nil {
		t.Errorf("stream_options must not also leak into Extra, got %v", req.Extra)
	}

	req, err = DecodeChatRequest(strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if req.IncludeStreamUsage {
		t.Error("IncludeStreamUsage must default to false when stream_options is omitted")
	}
}

func TestDecodeChatRequestRejectsMultiChoice(t *testing.T) {
	_, err := DecodeChatRequest(strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}],"n":2}`))
	if err == nil || !strings.Contains(err.Error(), "n is not supported") {
		t.Fatalf("want n rejection, got %v", err)
	}
	if _, err := DecodeChatRequest(strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}],"n":1}`)); err != nil {
		t.Fatalf("n:1 must pass, got %v", err)
	}
}

// TestDecodeChatRequestAcceptsFloatOne is a Protocol-translation Minor fix:
// "n" was validated by comparing raw JSON bytes against the literal string
// "1", so a semantically-identical single-choice request written as
// "n":1.0 (valid JSON, same value) was wrongly rejected.
func TestDecodeChatRequestAcceptsFloatOne(t *testing.T) {
	if _, err := DecodeChatRequest(strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}],"n":1.0}`)); err != nil {
		t.Fatalf("n:1.0 must pass (semantically n=1), got %v", err)
	}
}

// TestDecodeChatRequestRejectsNonNumericN proves the new semantic check
// still correctly rejects a malformed "n" rather than silently accepting it
// via a byte comparison that happens not to match.
func TestDecodeChatRequestRejectsNonNumericN(t *testing.T) {
	_, err := DecodeChatRequest(strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}],"n":"1"}`))
	if err == nil {
		t.Fatal("want an error for a non-numeric n, got nil")
	}
}

func TestDecodeChatRequestVisionContentArray(t *testing.T) {
	body := `{
		"model": "text",
		"messages": [{
			"role": "user",
			"content": [
				{"type": "text", "text": "what is in this image?"},
				{"type": "image_url", "image_url": {"url": "data:image/png;base64,AAAA"}}
			]
		}],
		"max_tokens": 30
	}`
	req, err := DecodeChatRequest(strings.NewReader(body))
	if err != nil {
		t.Fatalf("the exact bug report payload must decode without error, got: %v", err)
	}
	if len(req.Messages) != 1 {
		t.Fatalf("want 1 message, got %d", len(req.Messages))
	}
	m := req.Messages[0]
	if m.Content != "what is in this image?" {
		t.Errorf("Content = %q", m.Content)
	}
	if len(m.Images) != 1 || m.Images[0].URL != "data:image/png;base64,AAAA" {
		t.Errorf("Images = %+v", m.Images)
	}
}

func TestEncodeChatRequestMergesExtras(t *testing.T) {
	req := llm.ChatRequest{
		Model:    "m",
		Messages: []llm.Message{{Role: "user", Content: "hi"}},
		Extra: map[string]json.RawMessage{
			"top_p":           json.RawMessage(`0.9`),
			"response_format": json.RawMessage(`{"type":"json_object"}`),
		},
	}
	b, err := EncodeChatRequest(req, true)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if string(got["top_p"]) != `0.9` || string(got["response_format"]) != `{"type":"json_object"}` {
		t.Errorf("extras not merged: %s", b)
	}
	if string(got["model"]) != `"m"` || string(got["stream"]) != `true` {
		t.Errorf("owned fields damaged: %s", b)
	}
	if _, ok := got["stream_options"]; !ok {
		t.Errorf("gateway-imposed stream_options lost: %s", b)
	}
}
