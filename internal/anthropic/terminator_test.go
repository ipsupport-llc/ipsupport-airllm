package anthropic

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/llm"
)

func eventNames(buf *bytes.Buffer) []string {
	var names []string
	for _, ev := range strings.Split(strings.TrimSpace(buf.String()), "\n\n") {
		names = append(names, strings.TrimPrefix(strings.SplitN(ev, "\n", 2)[0], "event: "))
	}
	return names
}

// TestFinishEndsStreamWithoutUsage: an upstream that never reports usage
// must still get message_delta + message_stop, or an Anthropic client waits
// forever.
func TestFinishEndsStreamWithoutUsage(t *testing.T) {
	var buf bytes.Buffer
	sw := NewStreamWriter(&buf, func() {}, "msg_t", "m", 1)
	for _, c := range []llm.StreamChunk{{Role: "assistant"}, {Content: "hi"}, {FinishReason: "stop"}} {
		if err := sw.Chunk(c); err != nil {
			t.Fatal(err)
		}
	}
	if err := sw.Finish(); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(eventNames(&buf), ",")
	want := "message_start,content_block_start,content_block_delta,content_block_stop,message_delta,message_stop"
	if got != want {
		t.Fatalf("events = %s\nwant     %s", got, want)
	}
	if !strings.Contains(buf.String(), `"stop_reason":"end_turn"`) {
		t.Errorf("message_delta must carry the stop reason: %s", buf.String())
	}
}

// TestTerminalEventsExactlyOnce: with usage the terminal events go out on
// the usage chunk; Finish and a second usage report add nothing.
func TestTerminalEventsExactlyOnce(t *testing.T) {
	var buf bytes.Buffer
	sw := NewStreamWriter(&buf, func() {}, "msg_t", "m", 1)
	for _, c := range []llm.StreamChunk{{Content: "hi"}, {FinishReason: "stop"}, {Usage: &llm.Usage{CompletionTokens: 2}}, {Usage: &llm.Usage{CompletionTokens: 3}}} {
		if err := sw.Chunk(c); err != nil {
			t.Fatal(err)
		}
	}
	if err := sw.Finish(); err != nil {
		t.Fatal(err)
	}
	names := eventNames(&buf)
	if n := strings.Count(strings.Join(names, ","), "message_stop"); n != 1 || names[len(names)-1] != "message_stop" {
		t.Fatalf("want exactly one trailing message_stop, got %v", names)
	}
}

// TestFailEmitsErrorEventWithoutStop: a stream that breaks after it started
// ends with an error event and no message_stop.
func TestFailEmitsErrorEventWithoutStop(t *testing.T) {
	var buf bytes.Buffer
	sw := NewStreamWriter(&buf, func() {}, "msg_t", "m", 1)
	if err := sw.Chunk(llm.StreamChunk{Content: "par"}); err != nil {
		t.Fatal(err)
	}
	if err := sw.Fail("api_error", "upstream request failed"); err != nil {
		t.Fatal(err)
	}
	names := eventNames(&buf)
	if names[len(names)-1] != "error" || strings.Contains(buf.String(), "message_stop") {
		t.Fatalf("want a final error event and no message_stop, got %v", names)
	}
	if !strings.Contains(buf.String(), `{"error":{"message":"upstream request failed","type":"api_error"},"type":"error"}`) {
		t.Errorf("error payload: %s", buf.String())
	}
}

// TestFailAfterEndIsNoOp: once message_stop went out (or an error event),
// a late Fail must write nothing — nothing may follow the end of a message.
func TestFailAfterEndIsNoOp(t *testing.T) {
	var buf bytes.Buffer
	sw := NewStreamWriter(&buf, func() {}, "msg_t", "m", 1)
	for _, c := range []llm.StreamChunk{{Content: "hi"}, {FinishReason: "stop"}} {
		if err := sw.Chunk(c); err != nil {
			t.Fatal(err)
		}
	}
	if err := sw.Finish(); err != nil {
		t.Fatal(err)
	}
	before := buf.String()
	if err := sw.Fail("api_error", "late failure"); err != nil {
		t.Fatal(err)
	}
	if buf.String() != before {
		t.Fatalf("Fail after message_stop wrote more events: %q", strings.TrimPrefix(buf.String(), before))
	}

	var buf2 bytes.Buffer
	sw2 := NewStreamWriter(&buf2, func() {}, "msg_t", "m", 1)
	if err := sw2.Fail("api_error", "first"); err != nil {
		t.Fatal(err)
	}
	if err := sw2.Fail("api_error", "second"); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(buf2.String(), "event: error"); n != 1 {
		t.Fatalf("want exactly one error event, got %d: %s", n, buf2.String())
	}
}

func TestDecodeMapsStopSequencesTopPAndToolError(t *testing.T) {
	body := `{"model":"m","max_tokens":10,"top_p":0.9,"top_k":40,"stop_sequences":["END"],
	"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","is_error":true,"content":"boom"}]}]}`
	req, err := DecodeMessagesRequest(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if string(req.Extra["stop"]) != `["END"]` {
		t.Errorf("stop = %s", req.Extra["stop"])
	}
	if string(req.Extra["top_p"]) != `0.9` {
		t.Errorf("top_p = %s", req.Extra["top_p"])
	}
	if _, ok := req.Extra["top_k"]; ok {
		t.Error("top_k must not be forwarded to an OpenAI-shaped upstream")
	}
	var tool llm.Message
	for _, m := range req.Messages {
		if m.Role == "tool" {
			tool = m
		}
	}
	if tool.Content != toolErrorPrefix+"boom" {
		t.Errorf("tool content = %q, want the error marked", tool.Content)
	}
	// The flag is absent from a successful tool result.
	ok, _ := DecodeMessagesRequest(strings.NewReader(`{"model":"m","max_tokens":1,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"fine"}]}]}`))
	for _, m := range ok.Messages {
		if m.Role == "tool" && m.Content != "fine" {
			t.Errorf("unflagged tool result altered: %q", m.Content)
		}
	}
	b, _ := json.Marshal(req.Extra)
	if strings.Contains(string(b), "top_k") {
		t.Error("top_k leaked")
	}
}
