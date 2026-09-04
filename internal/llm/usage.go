package llm

import (
	"bytes"
	"encoding/json"
)

// Usage is token accounting for one response.
//
// The counts obey one invariant, established by Normalize and relied on by
// everything downstream: CompletionTokens is every token the model emitted and
// the vendor bills at the output rate — the visible answer plus whatever the
// model spent thinking — ReasoningTokens is the thinking share of it, and
// TotalTokens is PromptTokens plus CompletionTokens.
//
// Vendors do not agree on how to say that. OpenAI counts reasoning inside
// completion_tokens and breaks it out under completion_tokens_details; Vertex
// AI's OpenAI-compatible surface leaves it out of completion_tokens, so it
// shows up only as the difference between total_tokens and the parts.
// Normalize reconciles both to the invariant, which is why pricing and the
// rolling limits can go on reading two numbers and never learn which vendor
// answered.
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int

	// ReasoningTokens is the share of CompletionTokens the model spent
	// thinking. It is a breakdown of that number, not an addition to it:
	// charging for both double-counts. It exists as a field of its own
	// because the split is the whole answer to "how much am I paying to
	// think", which a single output count cannot give.
	ReasoningTokens int
}

// completionTokensDetails is OpenAI's breakdown of completion_tokens. Only the
// reasoning count is mapped; the rest of the vendor's fields are not used by
// this gateway and would be invented data if carried on the IR.
type completionTokensDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

// usageWire is the OpenAI wire shape of a usage object, on both the ingress
// this gateway serves and the upstreams it calls. It exists so Usage itself
// can hold a flat ReasoningTokens while the wire keeps the vendor's nesting.
type usageWire struct {
	PromptTokens     int                      `json:"prompt_tokens"`
	CompletionTokens int                      `json:"completion_tokens"`
	TotalTokens      int                      `json:"total_tokens"`
	Details          *completionTokensDetails `json:"completion_tokens_details,omitempty"`
}

// UnmarshalJSON reads a vendor's usage object and normalizes it, so every
// provider that decodes an OpenAI-shaped body — which is all of them — gets
// reasoning tokens accounted without a fix of its own.
func (u *Usage) UnmarshalJSON(b []byte) error {
	// By convention UnmarshalJSON leaves the receiver alone for a JSON null,
	// and this one must: the assignment below replaces the whole value, so
	// without this a `"usage": null` on one chunk would wipe counts already
	// decoded into that receiver.
	if string(bytes.TrimSpace(b)) == "null" {
		return nil
	}
	var w usageWire
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	*u = Usage{
		PromptTokens:     w.PromptTokens,
		CompletionTokens: w.CompletionTokens,
		TotalTokens:      w.TotalTokens,
	}
	if w.Details != nil {
		u.ReasoningTokens = w.Details.ReasoningTokens
	}
	u.Normalize()
	return nil
}

// MarshalJSON writes the usage object clients read. The reasoning count goes
// where OpenAI puts it, and only when there is one: a response that did no
// thinking serializes to exactly the three fields it always did, so nothing
// new appears in a client's parser from nowhere.
//
// Usage is the one IR type with a custom marshaller — Message deliberately has
// none, pinned by TestMessageHasNoCustomMarshalJSON — and the difference is
// worth stating. That rule exists because Message carries image bytes that
// must never be reached by a generic json.Marshal elsewhere in the gateway
// (the capture pipeline's, for one), so marshalling is kept dumb and the one
// place that needs the rich shape builds it by hand. Usage carries no such
// hazard: four small integers, safe to serialize anywhere. What it does carry
// is a shape a flat struct tag cannot spell, since the vendor nests the
// reasoning count one level down, and hand-building it in each of the two
// egresses that emit usage would be the duplication that rule is not about.
func (u Usage) MarshalJSON() ([]byte, error) {
	w := usageWire{
		PromptTokens:     u.PromptTokens,
		CompletionTokens: u.CompletionTokens,
		TotalTokens:      u.TotalTokens,
	}
	if u.ReasoningTokens > 0 {
		w.Details = &completionTokensDetails{ReasoningTokens: u.ReasoningTokens}
	}
	return json.Marshal(w)
}

// BilledTokens is the number of tokens the vendor charges for, and so the
// quantity one request counts against a key's rolling token caps: prompt plus
// completion, where completion already holds whatever the model spent
// thinking. ReasoningTokens is deliberately NOT added on top — it is a share of
// CompletionTokens, and adding it would charge the same tokens twice.
func (u Usage) BilledTokens() int64 { return int64(u.PromptTokens + u.CompletionTokens) }

// Normalize reconciles a vendor's usage report to the invariant documented on
// Usage. Three things can be wrong with a report as it arrives:
//
//   - The parts fall short of the total. The difference is output the vendor
//     billed but left out of completion_tokens, so it is folded into
//     CompletionTokens and, absent a larger number from the vendor, taken as
//     the reasoning count. Dropping it is what under-reported Gemini traffic
//     roughly 5.7x.
//
//     This rule is deliberately not keyed to a provider kind, because reading
//     one vendor's spelling is how the rest go on being under-reported. It
//     rests on an assumption worth stating: that a gap between the total and
//     the parts is thinking. It is on every upstream this gateway has met, and
//     the cost of being wrong is bounded — the gap is output the vendor's own
//     total says it charged for, so pricing it at the output rate is what the
//     invoice will say either way. Only the *label* would be wrong, and a
//     mislabelled breakdown is a better failure than a silent 5.7x shortfall.
//
//   - The reasoning count exceeds completion_tokens. Reasoning is billed as
//     output, so completion has to cover it however the vendor spelled the two.
//
//   - The total is missing or short. It is recomputed from the parts. A total
//     BELOW the parts is a vendor slip rather than a signal, so it is the total
//     that moves, never the parts — correcting it must not invent output to
//     charge for. This fires on non-reasoning responses too, for an upstream
//     that omits total_tokens: such a response used to reach the client as
//     total_tokens 0 beside non-zero parts. Nothing the gateway meters reads
//     TotalTokens, so this changes no ledger row, cost or cap — it only makes
//     the number a client already received add up.
//
// A report whose parts already sum to its total and that names no reasoning
// tokens passes through untouched, which is every non-reasoning response.
// Normalize is idempotent: usage crosses several decode layers and may meet it
// more than once.
func (u *Usage) Normalize() {
	if residual := u.TotalTokens - u.PromptTokens - u.CompletionTokens; residual > 0 {
		u.CompletionTokens += residual
		if residual > u.ReasoningTokens {
			u.ReasoningTokens = residual
		}
	}
	if u.ReasoningTokens > u.CompletionTokens {
		u.CompletionTokens = u.ReasoningTokens
	}
	if sum := u.PromptTokens + u.CompletionTokens; u.TotalTokens < sum {
		u.TotalTokens = sum
	}
}
