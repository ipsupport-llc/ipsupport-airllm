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

**OpenAI ingress** (`internal/openai/codec.go`, `DecodeChatRequest`):
the wire struct's `Content` field changes from `string` to
`json.RawMessage` (mirroring the pattern `internal/anthropic/codec.go`
already uses for exactly this reason). A new `convertContent` step tries,
in order: (1) unmarshal as a plain string — the overwhelmingly common
case, zero added parsing cost; (2) unmarshal as
`[]struct{Type string; Text string; ImageURL *struct{URL, Detail string}}`
— walk the parts, append `Text` values to the message's `Content` (joined,
matching Anthropic's existing `strings.Join(texts, "")` convention for
consistency across both ingress paths), append one `llm.Image{URL,
Detail}` per `image_url` part to `Images`. An unparseable body (neither
shape) is the existing invalid-request-body error path, unchanged.

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

**Egress to the real upstream** (`internal/openai/codec.go`,
`EncodeChatRequest`): a message with `len(Images) == 0` encodes `content`
exactly as today (a plain JSON string) — zero wire-format change for the
entire existing non-vision traffic, so no real backend that has ever
worked with this gateway can regress. A message with `len(Images) > 0`
encodes `content` as an array: one `{"type":"text","text":...}` part
(only when `Content != ""` — an image-only message omits the text part
entirely, matching what real OpenAI clients send) followed by one
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
`json:"-"`. `captureBody`'s `json.Marshal(payload)` — the one place in
this codebase that marshals `[]llm.Message` directly via reflection —
will never emit the field, present or future, without anyone needing to
remember to strip it at each call site. This mirrors the audio feature's
hard "no raw audio bytes ever persisted" constraint: no raw image bytes or
URLs are ever captured, logged, or stored, by construction of the type
itself rather than by a call-site convention.

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

- **`internal/openai`**: `DecodeChatRequest` — plain-string content
  (existing behavior, must be unaffected); array content with one text +
  one image part (text and image extracted correctly); array content with
  only an image part (empty `Content`, one `Image`); array content with
  multiple images (order preserved in `Images`); malformed content
  (neither string nor valid parts array) still produces the existing
  invalid-request error. `EncodeChatRequest` — a message with no images
  encodes `content` as a plain string (byte-identical to today's output);
  a message with one image encodes the two-part array shape with `detail`
  omitted when unset; a message with `Detail` set includes it.
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
