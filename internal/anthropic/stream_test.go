package anthropic

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/llm"
)

// collect runs chunks through a StreamWriter and returns the SSE events as
// (name, decoded-payload) pairs.
func collect(t *testing.T, chunks []llm.StreamChunk) []struct {
	Name string
	Data map[string]any
} {
	t.Helper()
	var buf bytes.Buffer
	sw := NewStreamWriter(&buf, func() {}, "msg_t", "m", 1)
	for _, c := range chunks {
		if err := sw.Chunk(c); err != nil {
			t.Fatal(err)
		}
	}
	var out []struct {
		Name string
		Data map[string]any
	}
	for _, ev := range strings.Split(strings.TrimSpace(buf.String()), "\n\n") {
		lines := strings.SplitN(ev, "\n", 2)
		name := strings.TrimPrefix(lines[0], "event: ")
		var data map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(lines[1], "data: ")), &data); err != nil {
			t.Fatalf("bad event payload: %v", err)
		}
		out = append(out, struct {
			Name string
			Data map[string]any
		}{name, data})
	}
	return out
}

func tcd(index int, id, name, args string) llm.ToolCallDelta {
	return llm.ToolCallDelta{Index: index, ID: id, Type: "function",
		Function: llm.FunctionCall{Name: name, Arguments: args}}
}

func TestParallelToolCallsGetSeparateBlocks(t *testing.T) {
	events := collect(t, []llm.StreamChunk{
		{Role: "assistant"},
		{ToolCalls: []llm.ToolCallDelta{tcd(0, "call_1", "file", `{"a":1}`)}},
		{ToolCalls: []llm.ToolCallDelta{tcd(1, "call_2", "file", `{"b":2}`)}},
		{FinishReason: "tool_calls"},
		{Usage: &llm.Usage{CompletionTokens: 5}},
	})
	var starts []map[string]any
	for _, e := range events {
		if e.Name == "content_block_start" {
			starts = append(starts, e.Data)
		}
	}
	if len(starts) != 2 {
		t.Fatalf("want 2 tool_use blocks, got %d", len(starts))
	}
	cb0 := starts[0]["content_block"].(map[string]any)
	cb1 := starts[1]["content_block"].(map[string]any)
	if cb0["id"] != "call_1" || cb1["id"] != "call_2" {
		t.Errorf("block ids: %v / %v", cb0["id"], cb1["id"])
	}
	if starts[0]["index"] == starts[1]["index"] {
		t.Errorf("blocks share an index: %v", starts[0]["index"])
	}
}

func TestFragmentedCallConcatenatesInOneBlock(t *testing.T) {
	events := collect(t, []llm.StreamChunk{
		{Role: "assistant"},
		{ToolCalls: []llm.ToolCallDelta{tcd(0, "call_1", "file", "")}},
		{ToolCalls: []llm.ToolCallDelta{tcd(0, "", "", `{"a"`)}},
		{ToolCalls: []llm.ToolCallDelta{tcd(0, "", "", `:1}`)}},
		{FinishReason: "tool_calls"},
		{Usage: &llm.Usage{CompletionTokens: 5}},
	})
	starts, sum := 0, ""
	for _, e := range events {
		switch e.Name {
		case "content_block_start":
			starts++
		case "content_block_delta":
			sum += e.Data["delta"].(map[string]any)["partial_json"].(string)
		}
	}
	if starts != 1 {
		t.Fatalf("want 1 block, got %d", starts)
	}
	if sum != `{"a":1}` {
		t.Errorf("partial_json sum = %q (a {} filler corrupts fragmented streams)", sum)
	}
}

func TestTextThenToolClosesTextBlock(t *testing.T) {
	events := collect(t, []llm.StreamChunk{
		{Role: "assistant"},
		{Content: "thinking..."},
		{ToolCalls: []llm.ToolCallDelta{tcd(0, "call_1", "file", `{"a":1}`)}},
		{FinishReason: "tool_calls"},
		{Usage: &llm.Usage{CompletionTokens: 5}},
	})
	var names []string
	for _, e := range events {
		names = append(names, e.Name)
	}
	joined := strings.Join(names, ",")
	want := "message_start,content_block_start,content_block_delta,content_block_stop,content_block_start,content_block_delta,content_block_stop,message_delta,message_stop"
	if joined != want {
		t.Errorf("event order:\n got %s\nwant %s", joined, want)
	}
}
