package llm

import (
	"sort"
	"strings"
)

// StreamAccumulator assembles the chunks of a streamed completion into the
// response the same call made without streaming would have returned: the
// role, the concatenated text, the tool calls put together from their
// deltas (OpenAI accumulation: the deltas sharing an Index are one call,
// whose id, type, name and extra content come from the delta that carries
// them first and whose arguments concatenate), the last finish reason and
// the last usage report. Reasoning text is not kept: a non-streamed
// response carries none either. The zero value is ready to use.
type StreamAccumulator struct {
	role     string
	content  strings.Builder
	calls    map[int]*ToolCall
	finish   string
	usage    *Usage
	sawDelta bool
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
	for _, d := range c.ToolCalls {
		if a.calls == nil {
			a.calls = map[int]*ToolCall{}
		}
		tc, ok := a.calls[d.Index]
		if !ok {
			tc = &ToolCall{}
			a.calls[d.Index] = tc
		}
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
	if c.FinishReason != "" {
		a.finish = c.FinishReason
	}
	if c.Usage != nil {
		u := *c.Usage
		a.usage = &u
	}
}

// Usage is the last usage report the stream carried, if any.
func (a *StreamAccumulator) Usage() (Usage, bool) {
	if a.usage == nil {
		return Usage{}, false
	}
	return *a.usage, true
}

// Response is the assembled response, with one choice — or none when the
// stream carried neither a delta nor usage, which is no answer at all. ID,
// Model and Created are left for the caller: a stream's chunks do not
// carry them in the IR.
func (a *StreamAccumulator) Response() ChatResponse {
	var resp ChatResponse
	if a.usage != nil {
		resp.Usage = *a.usage
	}
	if !a.sawDelta && a.usage == nil {
		return resp
	}
	msg := Message{Role: a.role, Content: a.content.String()}
	if msg.Role == "" {
		msg.Role = "assistant"
	}
	if len(a.calls) > 0 {
		idx := make([]int, 0, len(a.calls))
		for i := range a.calls {
			idx = append(idx, i)
		}
		sort.Ints(idx)
		for _, i := range idx {
			tc := *a.calls[i]
			if tc.Type == "" {
				tc.Type = "function"
			}
			msg.ToolCalls = append(msg.ToolCalls, tc)
		}
	}
	resp.Choices = []Choice{{Index: 0, Message: msg, FinishReason: a.finish}}
	return resp
}
