package llm

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestStreamAccumulatorAssemblesToolCalls(t *testing.T) {
	var a StreamAccumulator
	for _, c := range []StreamChunk{
		{Role: "assistant"},
		{Reasoning: "hmm"},
		{Content: "Hel"},
		{ToolCalls: []ToolCallDelta{{Index: 1, ID: "b", Function: FunctionCall{Name: "second", Arguments: `{"y":`}}}},
		{ToolCalls: []ToolCallDelta{{Index: 0, ID: "a", Type: "function", Function: FunctionCall{Name: "first"}, ExtraContent: json.RawMessage(`{"sig":1}`)}}},
		{Content: "lo", ToolCalls: []ToolCallDelta{{Index: 0, Function: FunctionCall{Arguments: `{"x":1}`}}}},
		{ToolCalls: []ToolCallDelta{{Index: 1, Function: FunctionCall{Arguments: `2}`}}}},
		{FinishReason: "tool_calls"},
		{Usage: &Usage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3}},
	} {
		a.Add(c)
	}
	got := a.Response()
	want := ChatResponse{
		Choices: []Choice{{Message: Message{Role: "assistant", Content: "Hello", ToolCalls: []ToolCall{
			{ID: "a", Type: "function", Function: FunctionCall{Name: "first", Arguments: `{"x":1}`}, ExtraContent: json.RawMessage(`{"sig":1}`)},
			{ID: "b", Type: "function", Function: FunctionCall{Name: "second", Arguments: `{"y":2}`}},
		}}, FinishReason: "tool_calls"}},
		Usage: Usage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("response =\n%+v\nwant\n%+v", got, want)
	}
	if u, ok := a.Usage(); !ok || u.TotalTokens != 3 {
		t.Errorf("usage = %+v %v", u, ok)
	}
}

func TestStreamAccumulatorWithoutAnythingIsNoAnswer(t *testing.T) {
	var a StreamAccumulator
	a.Add(StreamChunk{FinishReason: "stop"})
	if r := a.Response(); len(r.Choices) != 0 {
		t.Errorf("a stream with only a finish reason gave %+v, want no choices", r)
	}
	if _, ok := a.Usage(); ok {
		t.Error("usage reported for a stream that had none")
	}
	a.Add(StreamChunk{Role: "assistant"})
	if r := a.Response(); len(r.Choices) != 1 || r.Choices[0].Message.Role != "assistant" || r.Choices[0].FinishReason != "stop" {
		t.Errorf("an empty answer gave %+v, want one empty assistant choice", r)
	}
}
