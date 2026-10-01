package providers

import "github.com/ipsupport-llc/ipsupport-airllm/internal/llm"

// rewriteToolCalls returns in with edit applied to every message's tool
// calls. edit returns nil to leave a message as it is, or a fresh slice to
// replace its calls. The router reuses the caller's request across tiers, so
// the messages are copied on the first change and never edited in place.
func rewriteToolCalls(in llm.ChatRequest, edit func([]llm.ToolCall) []llm.ToolCall) llm.ChatRequest {
	out := in
	copied := false
	for i, m := range in.Messages {
		calls := edit(m.ToolCalls)
		if calls == nil {
			continue
		}
		if !copied {
			out.Messages = append([]llm.Message(nil), in.Messages...)
			copied = true
		}
		out.Messages[i].ToolCalls = calls
	}
	return out
}
