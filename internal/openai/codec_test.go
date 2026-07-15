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

func TestDecodeChatRequestRejectsMultiChoice(t *testing.T) {
	_, err := DecodeChatRequest(strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}],"n":2}`))
	if err == nil || !strings.Contains(err.Error(), "n is not supported") {
		t.Fatalf("want n rejection, got %v", err)
	}
	if _, err := DecodeChatRequest(strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}],"n":1}`)); err != nil {
		t.Fatalf("n:1 must pass, got %v", err)
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
