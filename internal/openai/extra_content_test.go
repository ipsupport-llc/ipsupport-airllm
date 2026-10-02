package openai

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// Gemini 3 on Vertex attaches a thought signature to a function call as
// extra_content, and refuses the next request unless it comes back. The
// gateway must carry it opaquely on every leg.
const vertexSignature = `{"google":{"thought_signature":"c2lnLUE="}}`

// toolCallExtras returns each tool call's extra_content, compacted, from a
// message object holding a tool_calls array.
func toolCallExtras(t *testing.T, msg map[string]json.RawMessage) []string {
	t.Helper()
	var calls []map[string]json.RawMessage
	if err := json.Unmarshal(msg["tool_calls"], &calls); err != nil {
		t.Fatalf("tool_calls: %v", err)
	}
	out := make([]string, len(calls))
	for i, c := range calls {
		raw, ok := c["extra_content"]
		if !ok {
			continue
		}
		var b bytes.Buffer
		if err := json.Compact(&b, raw); err != nil {
			t.Fatalf("extra_content: %v", err)
		}
		out[i] = b.String()
	}
	return out
}

func TestUpstreamResponseExtraContentReachesTheClient(t *testing.T) {
	upstream := `{"id":"r1","model":"google/gemini-3.5-flash-lite","choices":[{"index":0,"finish_reason":"tool_calls",
		"message":{"role":"assistant","tool_calls":[
			{"id":"c1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"},"extra_content":` + vertexSignature + `},
			{"id":"c2","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Tokyo\"}"}}]}}]}`

	resp, err := DecodeChatResponse(strings.NewReader(upstream))
	if err != nil {
		t.Fatal(err)
	}
	b, err := MarshalChatResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Choices []struct {
			Message map[string]json.RawMessage `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	extras := toolCallExtras(t, got.Choices[0].Message)
	if extras[0] != vertexSignature {
		t.Errorf("first call extra_content = %q, want %q", extras[0], vertexSignature)
	}
	if extras[1] != "" {
		t.Errorf("second call extra_content = %q, want absent", extras[1])
	}
}

func TestClientRequestExtraContentReachesUpstream(t *testing.T) {
	assistant := `{"role":"assistant","tool_calls":[
		{"id":"c1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"},"extra_content":` + vertexSignature + `},
		{"id":"c2","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Tokyo\"}"}}]`
	for name, msg := range map[string]string{
		"string content":     assistant + `,"content":""}`,
		"multi-part content": assistant + `,"content":[{"type":"text","text":"checking"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			body := `{"model":"m","messages":[{"role":"user","content":"weather in Paris and Tokyo?"},` + msg + `,
				{"role":"tool","tool_call_id":"c1","content":"18C"},{"role":"tool","tool_call_id":"c2","content":"24C"}]}`
			req, err := DecodeChatRequest(strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			b, err := EncodeChatRequest(req, false)
			if err != nil {
				t.Fatal(err)
			}
			var got struct {
				Messages []map[string]json.RawMessage `json:"messages"`
			}
			if err := json.Unmarshal(b, &got); err != nil {
				t.Fatal(err)
			}
			extras := toolCallExtras(t, got.Messages[1])
			if extras[0] != vertexSignature || extras[1] != "" {
				t.Errorf("extra_content = %q, want [%q, absent]", extras, vertexSignature)
			}
		})
	}
}

func TestUpstreamStreamExtraContentReachesTheClient(t *testing.T) {
	upstream := `{"choices":[{"delta":{"role":"assistant","tool_calls":[
		{"index":0,"id":"c1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"},"extra_content":` + vertexSignature + `},
		{"index":1,"id":"c2","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Tokyo\"}"}}]},"finish_reason":null}]}`

	c, err := ParseStreamChunk([]byte(upstream))
	if err != nil {
		t.Fatal(err)
	}
	b, err := MarshalStreamChunk(StreamMeta{ID: "id1", Model: "m", Created: 1}, c)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Choices []struct {
			Delta map[string]json.RawMessage `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	extras := toolCallExtras(t, got.Choices[0].Delta)
	if extras[0] != vertexSignature || extras[1] != "" {
		t.Errorf("extra_content = %q, want [%q, absent]", extras, vertexSignature)
	}
}
