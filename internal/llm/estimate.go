package llm

import "unicode/utf8"

// EstimateTokens is the gateway's crude token estimate of text — a quarter
// of its runes, the rule the Anthropic stream's input estimate and the mock
// use — for when an upstream reported no count: 0 for no text, at least 1
// for any.
func EstimateTokens(s string) int {
	n := utf8.RuneCountInString(s)
	if n == 0 {
		return 0
	}
	return max(n/4, 1)
}

// EstimatePromptTokens estimates the prompt req makes, by EstimateTokens over
// everything sent as text: each message's content, name and tool calls
// (names and arguments), and each tool's name, description and parameter
// schema. Images are not counted.
func EstimatePromptTokens(req ChatRequest) int {
	runes := 0
	add := func(s string) { runes += utf8.RuneCountInString(s) }
	for _, m := range req.Messages {
		add(m.Content)
		add(m.Name)
		for _, tc := range m.ToolCalls {
			add(tc.Function.Name)
			add(tc.Function.Arguments)
		}
	}
	for _, t := range req.Tools {
		add(t.Function.Name)
		add(t.Function.Description)
		add(string(t.Function.Parameters))
	}
	return max(runes/4, 1)
}
