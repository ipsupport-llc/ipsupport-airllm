package llm

import (
	"sort"
	"strings"
)

// StreamAccumulator assembles the chunks of a streamed completion into the
// response the same call made without streaming would have returned: the
// role, the concatenated text, the tool calls put together from their
// deltas, the last finish reason and the last usage report. Reasoning text
// is kept only for OutputText: a non-streamed response carries none. The
// zero value is ready to use.
//
// Tool calls follow OpenAI accumulation: the deltas sharing an Index are one
// call, whose id, type, name and extra content come from the delta that
// carries them first and whose arguments concatenate. A delta that carries
// an id different from the one its Index already holds, once that call has
// a name or arguments, starts a new call: some upstreams send every call at
// index 0.
type StreamAccumulator struct {
	role      string
	content   strings.Builder
	reasoning strings.Builder
	calls     []indexedCall
	current   map[int]int // Index -> position in calls of the call it now names
	finish    string
	usage     *Usage
	sawDelta  bool
}

type indexedCall struct {
	index int
	call  ToolCall
}

// Add folds one chunk into the response.
func (a *StreamAccumulator) Add(c StreamChunk) {
	if c.Role != "" || c.HasOutput() {
		a.sawDelta = true
	}
	if a.role == "" && c.Role != "" {
		a.role = c.Role
	}
	a.content.WriteString(c.Content)
	a.reasoning.WriteString(c.Reasoning)
	for _, d := range c.ToolCalls {
		a.addToolCall(d)
	}
	if c.FinishReason != "" {
		a.finish = c.FinishReason
	}
	if c.Usage != nil {
		u := *c.Usage
		a.usage = &u
	}
}

func (a *StreamAccumulator) addToolCall(d ToolCallDelta) {
	if a.current == nil {
		a.current = map[int]int{}
	}
	pos, ok := a.current[d.Index]
	if ok {
		cur := a.calls[pos].call
		if d.ID != "" && cur.ID != "" && d.ID != cur.ID && (cur.Function.Name != "" || cur.Function.Arguments != "") {
			ok = false
		}
	}
	if !ok {
		a.calls = append(a.calls, indexedCall{index: d.Index})
		pos = len(a.calls) - 1
		a.current[d.Index] = pos
	}
	tc := &a.calls[pos].call
	if tc.ID == "" {
		tc.ID = d.ID
	}
	if tc.Type == "" {
		tc.Type = d.Type
	}
	if tc.Function.Name == "" {
		tc.Function.Name = d.Function.Name
	}
	tc.Function.Arguments += d.Function.Arguments
	if len(tc.ExtraContent) == 0 && len(d.ExtraContent) > 0 {
		tc.ExtraContent = d.ExtraContent
	}
}

// Usage is the last usage report the stream carried, if any.
func (a *StreamAccumulator) Usage() (Usage, bool) {
	if a.usage == nil {
		return Usage{}, false
	}
	return *a.usage, true
}

// OutputText is everything the model produced, as text: its answer, its
// thinking, and its tool calls' names and arguments — what a token estimate
// of the completion is taken over. reasoning is the thinking alone.
func (a *StreamAccumulator) OutputText() (all, reasoning string) {
	var b strings.Builder
	b.WriteString(a.content.String())
	b.WriteString(a.reasoning.String())
	for _, c := range a.calls {
		b.WriteString(c.call.Function.Name)
		b.WriteString(c.call.Function.Arguments)
	}
	return b.String(), a.reasoning.String()
}

// Response is the assembled response, with one choice — or none when the
// stream carried no delta at all (only a finish reason or usage), which is
// no answer. ID, Model and Created are left for the caller: a stream's
// chunks do not carry them in the IR.
func (a *StreamAccumulator) Response() ChatResponse {
	var resp ChatResponse
	if a.usage != nil {
		resp.Usage = *a.usage
	}
	if !a.sawDelta {
		return resp
	}
	msg := Message{Role: a.role, Content: a.content.String()}
	if msg.Role == "" {
		msg.Role = "assistant"
	}
	calls := append([]indexedCall(nil), a.calls...)
	// By index; calls an upstream sent at the same index keep their order.
	sort.SliceStable(calls, func(i, j int) bool { return calls[i].index < calls[j].index })
	for _, c := range calls {
		tc := c.call
		if tc.Type == "" {
			tc.Type = "function"
		}
		msg.ToolCalls = append(msg.ToolCalls, tc)
	}
	resp.Choices = []Choice{{Index: 0, Message: msg, FinishReason: a.finish}}
	return resp
}
