# Vision / Multi-Part Image Content — Design

**Date:** 2026-09-04
**Status:** approved

## Problem

`llm.Message.Content` (`internal/llm/types.go`) is a plain `string`. Neither
ingress decoder handles OpenAI's standard multi-part message content
(`content: [{"type":"text",...}, {"type":"image_url",...}]}`), the format
every OpenAI-compatible client uses to send images for vision requests:

- **OpenAI ingress** (`internal/openai/codec.go`): `DecodeChatRequest`
  unmarshals `content` directly into a Go `string` field. An array value
  hard-fails: `json: cannot unmarshal array into Go struct field
  Message.messages.content of type string` — reported live against
  `POST /v1/chat/completions` on the `text` alias.
- **Anthropic ingress** (`internal/anthropic/codec.go`): `convertMessage`
  already parses Anthropic's own content-block array format (it has to,
  for text/tool_use/tool_result blocks), but its `switch blk.Type` has no
  `case "image"` — an image block matches no case and is **silently
  dropped**, no error, no data. Worse than the OpenAI path: a client
  believes its image was sent; it never was.

Both ingress protocols need real image support, not just error-message
polish.

## Architecture

**Scope-simplifying fact, confirmed by reading `internal/providers/registry_load.go`:**
this gateway has no `anthropic-direct` upstream client — the `kind` switch's
`default` branch (`slog.Warn("provider kind has no client yet; skipping")`)
means every real provider is built as `*OpenAICompat`, which always speaks
OpenAI wire format upstream, regardless of which protocol the client used
to connect. So there is exactly **one** egress encoder to teach about
images (`openai.EncodeChatRequest`), not two, and no OpenAI→Anthropic or
Anthropic→OpenAI image-shape translation is needed on the egress side —
only ingress-side normalization into one shared IR shape. (Response-direction
image encoding is a non-issue: LLM chat-completion responses are text only,
in both protocols.)

**IR** (`internal/llm/types.go`): `Message.Content string` keeps its exact
current meaning — the concatenated text of a message, nothing else. A new
field is added alongside it:

```go
// Image is one image attachment on a message, carried in OpenAI's
// image_url wire shape (a data: URI or a real http(s) URL) regardless of
// which ingress protocol produced it — the only upstream client this
// gateway has (OpenAICompat) speaks that shape natively.
type Image struct {
	URL    string // e.g. "data:image/png;base64,..." or "https://..."
	Detail string // optional OpenAI "detail": "auto"|"low"|"high"; empty = unset
}
```

```go
type Message struct {
	Role       string
	Content    string
	Images     []Image `json:"-"` // never serialized generically — see Persistence below
	ToolCalls  []ToolCall
	ToolCallID string
	// ...existing fields unchanged
}
```

`Images` is `nil` for every message today (zero behavior change for all
existing non-vision traffic) and populated only when an ingress decoder
finds an image part/block.

**Correction from an earlier draft of this section, found by reading the
actual code before writing the plan (not assumed):** unlike Anthropic's
ingress, which already has its own `messageWire`/`convertMessage`
translation layer, the OpenAI side has **no separate wire type for
messages at all** — `internal/openai/codec.go`'s `chatRequestWire.Messages
[]llm.Message` (ingress decode) and `internal/openai/client.go`'s
`upstreamChatRequest.Messages []llm.Message` (egress encode to the real
upstream) and `upstreamResponse.Choices[].Message llm.Message` (decoding
the upstream's own response) all reference `llm.Message` directly and rely
on `encoding/json`'s generic reflection-based (un)marshaling. Introducing
a fourth OpenAI-specific wire type here (duplicating Anthropic's pattern)
would work, but this package already has an established, narrower
precedent for exactly this shape of problem: `FunctionCall.UnmarshalJSON`
(`internal/llm/types.go`) tolerantly decodes a field that can arrive in
more than one JSON shape, directly on the IR type itself. `Message`
follows the same precedent instead of introducing wire-type duplication:

```go
// messageAlias has Message's exact field layout without its custom
// UnmarshalJSON, so the plain-string-content case (the overwhelming
// majority of traffic) decodes via ordinary struct reflection with zero
// added cost, and existing behavior is provably unchanged for it.
type messageAlias Message

// UnmarshalJSON tries the plain-string shape first (the common case,
// via messageAlias — a JSON array value for "content" fails this
// attempt with a type error, which is the intended discriminator);
// on failure, decodes Content as a multi-part array instead, joining
// text parts into Content and collecting image_url parts into Images.
func (m *Message) UnmarshalJSON(b []byte) error {
	// exact code in the plan
}
```

**Deliberately NOT adding a matching `MarshalJSON`** — a real risk was
caught while designing this (not left implicit): `internal/httpapi/capture_enqueue.go`'s
`captureBody` also marshals `[]llm.Message` generically
(`json.Marshal(payload)` where `payload.Messages = msgs`). If `Message`
had its own `MarshalJSON` that reads `m.Images` to build the wire array
(the encode-side mirror of the method above), that method would ALSO fire
for `captureBody`'s call — and since it reads `Images` directly via Go
code rather than through struct-tag reflection, the field's `json:"-"`
tag would provide **no protection at all** there: the raw image data
(potentially a full base64-embedded `data:` URI) would leak straight into
the capture pipeline, exactly the outcome the Persistence section below
rules out. `UnmarshalJSON` alone carries no such risk (it only reads
bytes in, never a Go field out), so only it goes on the shared IR type.

The encode-to-upstream direction — the one place that actually needs to
emit the array shape — gets its own conversion, scoped to
`internal/openai/client.go`'s `EncodeChatRequest` only, where the
`upstreamChatRequest.Messages []llm.Message` field is changed to a new
package-private wire type built explicitly from the IR:

```go
type openaiOutMessage struct {
	Role       string     `json:"role"`
	Content    any        `json:"content,omitempty"` // string, or []any of text/image_url parts
	Name       string     `json:"name,omitempty"`
	ToolCalls  []llm.ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

func toOpenAIOutMessage(m llm.Message) openaiOutMessage {
	// len(m.Images) == 0: Content = m.Content (string), byte-identical to
	// today's marshaled shape for every non-vision message.
	// len(m.Images) > 0: Content = []any{ text part (if Content != ""),
	// one image_url part per Image }, using the same wire shapes described
	// above.
	// (exact code in the plan)
}
```

`EncodeChatRequest` maps `req.Messages` through `toOpenAIOutMessage`
before assigning to `upstreamChatRequest.Messages`. This is the ONLY
place raw image data is ever read from `Images` to build output — a
single, narrow, auditable call site, not a method any future generic
`json.Marshal` on `llm.Message` could accidentally invoke.

`upstreamResponse.Choices[].Message llm.Message` (decoding the real
upstream's response) and `choiceWire.Message llm.Message` in
`MarshalChatResponse` (encoding the gateway's own response to the client)
are both unaffected: neither ever carries `Images` (models don't return
images in chat-completion responses in either protocol), so they
continue to marshal/unmarshal exactly as today — `UnmarshalJSON`'s
string-first branch always succeeds for a response, and with no custom
`MarshalJSON` on `Message`, the response encode path never even
considers the array shape.

**Anthropic ingress** (`internal/anthropic/codec.go`, `convertMessage`):
add `case "image":` to the existing block-type switch, alongside `"text"`,
`"tool_use"`, `"tool_result"`. Anthropic's block shape is
`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"..."}}`
(only the `base64` source type is in scope — matching "known cases only,"
Anthropic's URL-source image type, if the account/model even supports it,
is not covered by this pass, no incident or client need has surfaced it).
Convert to `base.Images = append(base.Images, llm.Image{URL: "data:" +
mediaType + ";base64," + data})` — a pure string-concatenation, no
network fetch, no re-encoding of the base64 payload itself.

**Real bug this change must not reintroduce**: `convertMessage`'s current
code only appends `base` to the output when `base.Content != "" ||
len(base.ToolCalls) > 0` (`codec.go:140`). An image-only message (no text,
no tool call) has both empty — under the unmodified condition it would be
silently dropped, the exact same failure class this whole design exists to
fix, just moved one field over. This condition MUST become `base.Content
!= "" || len(base.ToolCalls) > 0 || len(base.Images) > 0`. This is a
required code change, not only a test case.

**Egress to the real upstream**: via `toOpenAIOutMessage` above. A
message with `len(Images) == 0` encodes `content` exactly as today (a
plain JSON string) — zero wire-format change for the entire existing
non-vision traffic, so no real backend that has ever worked with this
gateway can regress. A message with `len(Images) > 0` encodes `content`
as an array: one `{"type":"text","text":...}` part (only when `Content
!= ""` — an image-only message omits the text part entirely, matching
what real OpenAI clients send) followed by one
`{"type":"image_url","image_url":{"url":...,"detail":...}}` per image
(`detail` omitted when empty, matching OpenAI's own optional field).
Ordering is always text-then-images regardless of the original client's
part ordering — acceptable: vision models don't depend on interleaving
order, and no real client sends anything OpenAI's own docs don't already
describe this same way.

**DLP** (`internal/httpapi/dlp.go`): no code change. `dlpEnforce` already
scans `req.Messages[i].Content` — still exactly the joined text, exactly
as before. `Images` isn't a string, isn't touched by regex/entropy
scanning, and DLP's own design principle ("prompts only, text only") holds
by construction, not by a new check someone could forget to add.

**Persistence** (`internal/httpapi/capture_enqueue.go` and anywhere else
`llm.Message` might ever be JSON-marshaled generically): `Images` carries
`json:"-"`, and — precisely because `Message` gets no `MarshalJSON` of its
own (see above) — this tag is actually load-bearing: `captureBody`'s
`json.Marshal(payload)` (`payload.Messages = msgs`) falls through to
ordinary struct-tag reflection, which honors `json:"-"` and skips the
field unconditionally. Present or future generic marshal of `[]llm.Message`
anywhere in this codebase gets the same guarantee, without anyone needing
to remember to strip it at each call site. This mirrors the audio
feature's hard "no raw audio bytes ever persisted" constraint: no raw
image bytes or URLs are ever captured, logged, or stored, by construction
of the type itself rather than by a call-site convention.

**Vision-capability mismatch** (known, accepted limitation, matching
Ruling 7 from the audio feature): this gateway cannot locally verify that
a specific alias's target model is actually vision-capable before making
the call — `OpenAICompat` is the one shared client type for every real
provider kind, with no local capability registry. A request with an image
sent to a text-only model reaches the real upstream and gets whatever
error that vendor returns (typically a normal `400`), rather than a clean
local check. Not mitigated in this pass — same reasoning as Ruling 7:
building a real per-model capability registry is materially larger scope
than the reported problem, for a failure mode that's already an honest,
if unpolished, error rather than a crash or silent wrong answer.

## Testing

- **`internal/llm`**: `Message.UnmarshalJSON` — plain-string content
  (existing behavior, must be unaffected, including `content` absent
  entirely and `content: null`); array content with one text + one image
  part (text and image extracted correctly); array content with only an
  image part (empty `Content`, one `Image`); array content with multiple
  images (order preserved in `Images`); malformed content (neither string
  nor a valid parts array) still produces a decode error. Also confirm
  `Message` has no `MarshalJSON` method — a one-line test that
  `json.Marshal(Message{Content: "hi"})` produces exactly
  `{"role":"","content":"hi"}` (or whatever the struct-tag-derived shape
  is) pins this as a deliberate, checked property, not an assumption.
- **`internal/openai`**: one `DecodeChatRequest` test using the bug
  report's own literal JSON body end to end (the real regression case),
  confirming no error and the expected `Content`/`Images` on the decoded
  message. `toOpenAIOutMessage`/`EncodeChatRequest` (`client.go`) — a
  message with no images encodes `content` as a plain string
  (byte-identical to today's output for a no-image message — a useful
  regression assertion is comparing against the pre-change marshaled
  bytes); a message with one image encodes the two-part array shape with
  `detail` omitted when unset; a message with `Detail` set includes it;
  an image-only message (empty `Content`) omits the text part.
  `DecodeChatResponse` doesn't need a dedicated new test — it does
  nothing content-shape-specific of its own, and real upstream responses
  never carry array content in practice.
- **`internal/anthropic`**: `convertMessage` — an image block converts to
  the expected `data:` URI in `Images`, alongside existing text-block
  behavior; a message that is ONLY an image block (no text, no tool
  calls) must still produce one IR message (today's code only appends
  `base` when `Content != "" || len(ToolCalls) > 0` — this condition needs
  a matching `|| len(Images) > 0`, or an image-only message would be
  silently dropped exactly like today's bug, just one field later).
- **`internal/httpapi`**: a focused test confirming `dlpEnforce`/`dlpScanText`-style
  scanning never sees `Images` (construct a message with a planted secret
  in `Content` AND a real-looking data URI in `Images`, confirm the
  DLP finding count/labels are identical to the same message with
  `Images` unset — proves the field is invisible to scanning, not just
  "probably fine"). A focused test confirming `captureBody`'s JSON output
  for a message with `Images` set contains no image data at all (marshal,
  then assert the output doesn't contain the planted URL substring).
- **`internal/providers`**: `Mock.Chat` gains a small, deterministic
  extension — when the incoming request's last user message has
  `len(Images) > 0`, append `fmt.Sprintf(" (with %d image(s) attached)",
  len(images))` to its existing echoed content. Cheap, fully within this
  codebase's control, and it's what makes the live-verification step below
  possible without needing a real vision-capable backend configured in dev.
- **Live** (compose): send the bug report's own example payload (the
  `text`/`image_url` two-part content array) through a `Mock`-backed alias
  end to end and confirm `200` with the image-count echoed in the
  response — not the original unmarshal error. Also send a plain-string-content
  request through the same alias and confirm it's completely unaffected
  (regression check for the overwhelming majority of existing traffic).

## Out of scope

- Cross-protocol image-shape translation (Anthropic-shaped upstream
  receiving an OpenAI-originated image, or vice versa) — moot, since no
  `anthropic-direct` upstream client exists in this codebase today; revisit
  if one is ever added.
- Anthropic's `image` source type `"url"` (fetch-by-URL rather than inline
  base64) — not covered; add if/when a real client/incident needs it,
  matching this session's "known cases only" discipline.
- Any local verification that an alias's target model is actually
  vision-capable — accepted limitation, mirrors Ruling 7.
- Fetching/proxying/re-encoding remote `image_url` URLs server-side — the
  gateway forwards whatever URL shape the client sent, verbatim; the real
  upstream is responsible for fetching it if it's not a `data:` URI.
- Response-direction (egress-to-client) image content — chat completion
  responses are text only in both protocols; not applicable.
- Any change to pricing/token-accounting for image content — providers
  report their own usage; this pass doesn't add local image-token
  estimation.
