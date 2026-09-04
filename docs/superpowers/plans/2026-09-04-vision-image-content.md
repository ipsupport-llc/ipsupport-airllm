# Vision Image Content Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let both ingress protocols (OpenAI, Anthropic) accept standard multi-part message content (text + images) instead of hard-failing (OpenAI) or silently dropping the image (Anthropic), and forward images correctly to the one real upstream client this gateway has.

**Architecture:** `llm.Message` gains an `Images []Image` field (never generically JSON-serialized — `json:"-"`) alongside its unchanged `Content string`. A custom `Message.UnmarshalJSON` (decode-only — no matching `MarshalJSON`, deliberately, to avoid a capture-pipeline leak risk found during design) lets the OpenAI ingress path parse array-shaped content automatically, since `llm.Message` is unmarshaled directly there with no separate wire type. The Anthropic ingress path (which already has its own `messageWire`/`convertMessage` translation layer) gets one new `case "image":` in its existing block-type switch. The one real upstream client (`OpenAICompat`) gets a small, narrowly-scoped wire type in `client.go` that's the only place raw image data is ever read to build output.

**Tech Stack:** Go stdlib `encoding/json` only — no new dependencies.

**Spec:** docs/superpowers/specs/2026-09-04-vision-image-content-design.md

## Global Constraints

- `Message.Content` keeps its exact current meaning (joined text only) — zero behavior change for any call site that only reads `Content` (DLP scanning, capture, Mock's token/echo logic).
- `Message` gets `UnmarshalJSON` but explicitly NOT `MarshalJSON` — a custom `MarshalJSON` reading `Images` directly would bypass `Images`' `json:"-"` tag for `captureBody`'s generic marshal, leaking raw image data into the capture pipeline. The encode-to-upstream direction lives in its own narrow type in `internal/openai/client.go` instead.
- Only two content-part shapes are covered: OpenAI's `image_url` (any URL string, including `data:` URIs, forwarded verbatim) and Anthropic's `image` block with `source.type == "base64"`. No URL-source Anthropic images, no other vendor shapes — known cases only.
- No cross-protocol image-shape translation on egress — this codebase has no `anthropic-direct` upstream client, so the only real egress path is OpenAI wire format, regardless of which protocol the client connected with.
- No local verification that a target model is vision-capable — accepted limitation, matches the audio feature's Ruling 7.
- `gofmt -l .` clean before every commit; `go build ./... && go vet ./... && go test ./...` green.

---

### Task 1: IR — `Image` type, `Message.Images`, `Message.UnmarshalJSON`

**Files:**
- Modify: `internal/llm/types.go`
- Test: `internal/llm/types_test.go` (extend existing file)

**Interfaces:**
- Produces (consumed by Tasks 2, 3, 4):
  - `llm.Image{URL string, Detail string}`
  - `llm.Message.Images []Image` (new field, `json:"-"`)
  - `Message.UnmarshalJSON` — makes every existing `[]llm.Message` decode point (OpenAI ingress, upstream response decode) automatically tolerant of array-shaped `content`; no other task needs to call anything new to get this — it's automatic via `encoding/json`.

- [ ] **Step 1: Write the failing tests**

Add to `internal/llm/types_test.go` (same file, same imports already present — `encoding/json`, `testing`):

```go
func TestMessageUnmarshalPlainStringContent(t *testing.T) {
	var m Message
	if err := json.Unmarshal([]byte(`{"role":"user","content":"hi"}`), &m); err != nil {
		t.Fatal(err)
	}
	if m.Role != "user" || m.Content != "hi" || len(m.Images) != 0 {
		t.Errorf("got %+v", m)
	}
}

func TestMessageUnmarshalContentAbsentOrNull(t *testing.T) {
	for _, body := range []string{`{"role":"assistant","tool_calls":[]}`, `{"role":"user","content":null}`} {
		var m Message
		if err := json.Unmarshal([]byte(body), &m); err != nil {
			t.Fatalf("%s: %v", body, err)
		}
		if m.Content != "" || len(m.Images) != 0 {
			t.Errorf("%s: got %+v, want empty Content and no Images", body, m)
		}
	}
}

func TestMessageUnmarshalMultiPartTextAndImage(t *testing.T) {
	body := `{"role":"user","content":[
		{"type":"text","text":"what is in this image?"},
		{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}
	]}`
	var m Message
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatal(err)
	}
	if m.Content != "what is in this image?" {
		t.Errorf("Content = %q", m.Content)
	}
	if len(m.Images) != 1 || m.Images[0].URL != "data:image/png;base64,AAAA" {
		t.Errorf("Images = %+v", m.Images)
	}
}

func TestMessageUnmarshalImageOnlyContent(t *testing.T) {
	body := `{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/a.png","detail":"high"}}]}`
	var m Message
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatal(err)
	}
	if m.Content != "" {
		t.Errorf("Content = %q, want empty", m.Content)
	}
	if len(m.Images) != 1 || m.Images[0].URL != "https://example.com/a.png" || m.Images[0].Detail != "high" {
		t.Errorf("Images = %+v", m.Images)
	}
}

func TestMessageUnmarshalMultipleImages(t *testing.T) {
	body := `{"role":"user","content":[
		{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}},
		{"type":"text","text":"compare these"},
		{"type":"image_url","image_url":{"url":"data:image/png;base64,BBBB"}}
	]}`
	var m Message
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatal(err)
	}
	if m.Content != "compare these" {
		t.Errorf("Content = %q", m.Content)
	}
	if len(m.Images) != 2 || m.Images[0].URL != "data:image/png;base64,AAAA" || m.Images[1].URL != "data:image/png;base64,BBBB" {
		t.Errorf("Images order/content wrong: %+v", m.Images)
	}
}

func TestMessageUnmarshalMalformedContentErrors(t *testing.T) {
	var m Message
	err := json.Unmarshal([]byte(`{"role":"user","content":42}`), &m)
	if err == nil {
		t.Error("expected an error for content that is neither a string nor a parts array")
	}
}

func TestMessageHasNoCustomMarshalJSON(t *testing.T) {
	b, err := json.Marshal(Message{Role: "user", Content: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"role":"user","content":"hi"}` {
		t.Errorf("got %s — Message must marshal via plain struct-tag reflection, no custom MarshalJSON", b)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd /home/roman220/gh/ipsupport-airllm && go test ./internal/llm/... -run TestMessage -v`
Expected: FAIL — `Image` type and `Message.Images` don't exist yet (compile error).

- [ ] **Step 3: Implement**

In `internal/llm/types.go`, add this new type right after the existing `Message` struct (currently lines 10-16):

```go
// Image is one image attachment on a message, carried in OpenAI's
// image_url wire shape (a data: URI or a real http(s) URL) regardless of
// which ingress protocol produced it — the only upstream client this
// gateway has (OpenAICompat) speaks that shape natively.
type Image struct {
	URL    string
	Detail string // optional OpenAI "detail": "auto"|"low"|"high"; empty = unset
}
```

Change the `Message` struct from:

```go
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	Name       string     `json:"name,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}
```

to:

```go
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	Images     []Image    `json:"-"` // never serialized generically — see Persistence in the design spec
	Name       string     `json:"name,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}
```

Add this immediately after the (now-modified) `Message` struct — a `messageAlias` type and the `UnmarshalJSON` method. **Do not add a `MarshalJSON` method** — this is deliberate, see the Global Constraints above and the design spec's "Deliberately NOT adding a matching MarshalJSON" section:

```go
// messageAlias has Message's exact field layout without a custom
// UnmarshalJSON, so the plain-string-content case (the overwhelming
// majority of traffic) decodes via ordinary struct reflection with zero
// added cost.
type messageAlias Message

// contentPart is one element of an OpenAI multi-part content array.
type contentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL *struct {
		URL    string `json:"url"`
		Detail string `json:"detail,omitempty"`
	} `json:"image_url,omitempty"`
}

// UnmarshalJSON tries the plain-string content shape first (the common
// case, via messageAlias — a JSON array value for "content" fails this
// attempt with a type error, which is the intended discriminator); on
// failure, decodes Content as a multi-part array instead, joining text
// parts into Content and collecting image_url parts into Images.
func (m *Message) UnmarshalJSON(b []byte) error {
	var direct messageAlias
	if err := json.Unmarshal(b, &direct); err == nil {
		*m = Message(direct)
		return nil
	}

	var w struct {
		Role       string          `json:"role"`
		Content    json.RawMessage `json:"content"`
		Name       string          `json:"name"`
		ToolCalls  []ToolCall      `json:"tool_calls"`
		ToolCallID string          `json:"tool_call_id"`
	}
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	m.Role, m.Name, m.ToolCalls, m.ToolCallID = w.Role, w.Name, w.ToolCalls, w.ToolCallID

	var parts []contentPart
	if err := json.Unmarshal(w.Content, &parts); err != nil {
		return err
	}
	var texts []string
	for _, p := range parts {
		switch p.Type {
		case "text":
			texts = append(texts, p.Text)
		case "image_url":
			if p.ImageURL != nil {
				m.Images = append(m.Images, Image{URL: p.ImageURL.URL, Detail: p.ImageURL.Detail})
			}
		}
	}
	m.Content = strings.Join(texts, "")
	return nil
}
```

Add `"strings"` to `internal/llm/types.go`'s import block (currently only `"encoding/json"`):

```go
import (
	"encoding/json"
	"strings"
)
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/llm/... -v`
Expected: PASS — all new tests plus the existing `TestFunctionCallUnmarshalTolerant`/`TestFunctionCallMarshalStaysString` (unaffected).

- [ ] **Step 5: Run the full build (nothing else should break yet)**

Run: `gofmt -l . && go build ./... && go vet ./...`
Expected: clean. `go build` will fail if any other package constructs a `Message{...}` with unkeyed positional fields (the new `Images` field would shift positions) — if it does, that's a pre-existing bug this step surfaces, not something to fix in this task; report it if found rather than silently patching call sites outside this task's scope.

- [ ] **Step 6: Commit**

```bash
git add internal/llm/types.go internal/llm/types_test.go
git commit -m "feat(llm): Image type + Message.Images + tolerant content UnmarshalJSON"
```

---

### Task 2: OpenAI egress — encode images to the real upstream

**Files:**
- Modify: `internal/openai/client.go`
- Test: `internal/openai/client_test.go` (new file)
- Test: `internal/openai/codec_test.go` (extend — the actual reported-bug regression test)

**Interfaces:**
- Consumes: `llm.Image`, `Message.Images`, `Message.UnmarshalJSON` (Task 1)
- Produces: nothing new consumed by later tasks — this task's deliverable is self-contained (the upstream encode path).

- [ ] **Step 1: Write the failing tests**

Create `internal/openai/client_test.go`:

```go
package openai

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/llm"
)

func TestEncodeChatRequestNoImagesUnchanged(t *testing.T) {
	req := llm.ChatRequest{Model: "m", Messages: []llm.Message{{Role: "user", Content: "hi"}}}
	b, err := EncodeChatRequest(req, false)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	msgs := got["messages"].([]any)
	msg := msgs[0].(map[string]any)
	if content, ok := msg["content"].(string); !ok || content != "hi" {
		t.Errorf("content = %#v, want plain string \"hi\"", msg["content"])
	}
}

func TestEncodeChatRequestWithOneImage(t *testing.T) {
	req := llm.ChatRequest{Model: "m", Messages: []llm.Message{{
		Role: "user", Content: "what is this?",
		Images: []llm.Image{{URL: "data:image/png;base64,AAAA"}},
	}}}
	b, err := EncodeChatRequest(req, false)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	msg := got["messages"].([]any)[0].(map[string]any)
	parts, ok := msg["content"].([]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("content = %#v, want a 2-part array", msg["content"])
	}
	textPart := parts[0].(map[string]any)
	if textPart["type"] != "text" || textPart["text"] != "what is this?" {
		t.Errorf("text part = %#v", textPart)
	}
	imgPart := parts[1].(map[string]any)
	if imgPart["type"] != "image_url" {
		t.Errorf("image part type = %#v", imgPart["type"])
	}
	imgURL := imgPart["image_url"].(map[string]any)
	if imgURL["url"] != "data:image/png;base64,AAAA" {
		t.Errorf("image url = %#v", imgURL["url"])
	}
	if _, hasDetail := imgURL["detail"]; hasDetail {
		t.Error("detail should be omitted when unset")
	}
}

func TestEncodeChatRequestImageWithDetail(t *testing.T) {
	req := llm.ChatRequest{Model: "m", Messages: []llm.Message{{
		Role: "user", Images: []llm.Image{{URL: "https://x/a.png", Detail: "high"}},
	}}}
	b, err := EncodeChatRequest(req, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"detail":"high"`) {
		t.Errorf("expected detail to be included, got %s", b)
	}
}

func TestEncodeChatRequestEmptyContentNoImagesOmitsContentKey(t *testing.T) {
	// A tool-calls-only assistant message: Content == "", no Images.
	// Today's plain `Content string `json:"content,omitempty"`` field
	// omits the key entirely in this case — this must not regress once
	// Content becomes `any` on the wire type (see toOpenAIOutMessage's
	// comment on why a naive assignment would break this).
	req := llm.ChatRequest{Model: "m", Messages: []llm.Message{{
		Role: "assistant",
		ToolCalls: []llm.ToolCall{{ID: "1", Type: "function", Function: llm.FunctionCall{Name: "f", Arguments: "{}"}}},
	}}}
	b, err := EncodeChatRequest(req, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"content"`) {
		t.Errorf("content key must be omitted entirely for empty content with no images, got: %s", b)
	}
}

func TestEncodeChatRequestImageOnlyOmitsTextPart(t *testing.T) {
	req := llm.ChatRequest{Model: "m", Messages: []llm.Message{{
		Role: "user", Images: []llm.Image{{URL: "https://x/a.png"}},
	}}}
	b, err := EncodeChatRequest(req, false)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	parts := got["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if len(parts) != 1 {
		t.Fatalf("want exactly 1 part (image only, no text part), got %d: %#v", len(parts), parts)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/openai/... -run TestEncodeChatRequest -v`
Expected: FAIL — `EncodeChatRequest` still marshals `[]llm.Message` directly (no image-aware encoding yet), so an image message's `content` still encodes as `""` (Content is empty, Images is ignored) instead of the 2-part array.

- [ ] **Step 3: Implement**

In `internal/openai/client.go`, add this new type and function right after the existing `streamOptions` type (before `upstreamChatRequest`):

```go
// openaiOutMessage is the wire shape for one message sent to the real
// upstream. Content is `any` because it's a plain string for the common
// no-image case (byte-identical to today's output) or a []any of
// text/image_url parts when the message carries images. This is the
// ONLY place raw image data is ever read from Message.Images to build
// output — deliberately not a method on llm.Message itself, so no
// future generic json.Marshal on llm.Message (e.g. the capture
// pipeline's) can accidentally invoke it. See the design spec's
// "Deliberately NOT adding a matching MarshalJSON" section.
type openaiOutMessage struct {
	Role       string         `json:"role"`
	Content    any            `json:"content,omitempty"`
	Name       string         `json:"name,omitempty"`
	ToolCalls  []llm.ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type outTextPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type outImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

type outImagePart struct {
	Type     string      `json:"type"`
	ImageURL outImageURL `json:"image_url"`
}

func toOpenAIOutMessage(m llm.Message) openaiOutMessage {
	out := openaiOutMessage{Role: m.Role, Name: m.Name, ToolCalls: m.ToolCalls, ToolCallID: m.ToolCallID}
	if len(m.Images) == 0 {
		// IMPORTANT: only assign when non-empty. `Content any` with
		// `omitempty` only omits a truly-nil interface — assigning the
		// empty string "" (even though it IS the empty string) would
		// leave the interface non-nil, so `omitempty` would NOT omit it,
		// and a tool-calls-only assistant message (Content == "") would
		// regress from omitting "content" entirely (today's exact
		// behavior, since Message.Content is a plain string field there)
		// to emitting `"content":""`. Leaving out.Content as its zero
		// value (untyped nil) when Content == "" preserves today's
		// omitted-field behavior exactly.
		if m.Content != "" {
			out.Content = m.Content
		}
		return out
	}
	var parts []any
	if m.Content != "" {
		parts = append(parts, outTextPart{Type: "text", Text: m.Content})
	}
	for _, img := range m.Images {
		parts = append(parts, outImagePart{Type: "image_url", ImageURL: outImageURL{URL: img.URL, Detail: img.Detail}})
	}
	out.Content = parts
	return out
}
```

Change the `upstreamChatRequest` struct's `Messages` field from:

```go
type upstreamChatRequest struct {
	Model             string          `json:"model"`
	Messages          []llm.Message   `json:"messages"`
```

to:

```go
type upstreamChatRequest struct {
	Model             string             `json:"model"`
	Messages          []openaiOutMessage `json:"messages"`
```

In `EncodeChatRequest`, change:

```go
	u := upstreamChatRequest{
		Model:             req.Model,
		Messages:          req.Messages,
```

to:

```go
	outMsgs := make([]openaiOutMessage, len(req.Messages))
	for i, m := range req.Messages {
		outMsgs[i] = toOpenAIOutMessage(m)
	}
	u := upstreamChatRequest{
		Model:             req.Model,
		Messages:          outMsgs,
```

(Everything else in `EncodeChatRequest` — `Tools`, `ToolChoice`, and so on down through the `Extra` merge — is unchanged.)

`upstreamResponse.Choices[].Message llm.Message` and `DecodeChatResponse` are NOT touched by this task — real upstream responses never carry array content, and this field still decodes via `Message.UnmarshalJSON` from Task 1 automatically (its string-first branch always succeeds for a response).

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/openai/... -run TestEncodeChatRequest -v`
Expected: PASS (5 tests)

- [ ] **Step 5: Add the actual bug-report regression test**

Add to the existing `internal/openai/codec_test.go` (same file, same imports already present):

```go
func TestDecodeChatRequestVisionContentArray(t *testing.T) {
	body := `{
		"model": "text",
		"messages": [{
			"role": "user",
			"content": [
				{"type": "text", "text": "what is in this image?"},
				{"type": "image_url", "image_url": {"url": "data:image/png;base64,AAAA"}}
			]
		}],
		"max_tokens": 30
	}`
	req, err := DecodeChatRequest(strings.NewReader(body))
	if err != nil {
		t.Fatalf("the exact bug report payload must decode without error, got: %v", err)
	}
	if len(req.Messages) != 1 {
		t.Fatalf("want 1 message, got %d", len(req.Messages))
	}
	m := req.Messages[0]
	if m.Content != "what is in this image?" {
		t.Errorf("Content = %q", m.Content)
	}
	if len(m.Images) != 1 || m.Images[0].URL != "data:image/png;base64,AAAA" {
		t.Errorf("Images = %+v", m.Images)
	}
}
```

- [ ] **Step 6: Run test to verify it passes**

Run: `go test ./internal/openai/... -run TestDecodeChatRequestVisionContentArray -v`
Expected: PASS — this is the literal repro of the reported bug; before Task 1 existed, this test would have failed with exactly the reported error.

- [ ] **Step 7: Run the full openai package suite**

Run: `gofmt -l . && go build ./... && go vet ./... && go test ./internal/openai/... -v`
Expected: all green, including every pre-existing test (`TestDecodeChatRequestExtractsExtras`, the `stream_test.go` tests, etc.) unaffected.

- [ ] **Step 8: Commit**

```bash
git add internal/openai/client.go internal/openai/client_test.go internal/openai/codec_test.go
git commit -m "feat(openai): encode images to the real upstream; regression test for the reported bug"
```

---

### Task 3: Anthropic ingress — image content blocks

**Files:**
- Modify: `internal/anthropic/codec.go`
- Test: `internal/anthropic/codec_test.go` (extend existing file)

**Interfaces:**
- Consumes: `llm.Image`, `Message.Images` (Task 1)

- [ ] **Step 1: Write the failing tests**

Add to `internal/anthropic/codec_test.go` (same file, same imports already present — `strings`, `testing`, `llm`):

```go
func TestDecodeImageBlockContent(t *testing.T) {
	body := `{
		"model": "claude-x",
		"max_tokens": 100,
		"messages": [{
			"role": "user",
			"content": [
				{"type": "text", "text": "what is this?"},
				{"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "AAAA"}}
			]
		}]
	}`
	req, err := DecodeMessagesRequest(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Messages) != 1 {
		t.Fatalf("want 1 message, got %d", len(req.Messages))
	}
	m := req.Messages[0]
	if m.Content != "what is this?" {
		t.Errorf("Content = %q", m.Content)
	}
	if len(m.Images) != 1 || m.Images[0].URL != "data:image/png;base64,AAAA" {
		t.Errorf("Images = %+v", m.Images)
	}
}

func TestDecodeImageOnlyMessageNotDropped(t *testing.T) {
	body := `{
		"model": "claude-x",
		"max_tokens": 100,
		"messages": [{
			"role": "user",
			"content": [{"type": "image", "source": {"type": "base64", "media_type": "image/jpeg", "data": "BBBB"}}]
		}]
	}`
	req, err := DecodeMessagesRequest(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Messages) != 1 {
		t.Fatalf("an image-only message must not be silently dropped — got %d messages", len(req.Messages))
	}
	if req.Messages[0].Content != "" {
		t.Errorf("Content = %q, want empty", req.Messages[0].Content)
	}
	if len(req.Messages[0].Images) != 1 || req.Messages[0].Images[0].URL != "data:image/jpeg;base64,BBBB" {
		t.Errorf("Images = %+v", req.Messages[0].Images)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/anthropic/... -run TestDecodeImage -v`
Expected: FAIL — `TestDecodeImageBlockContent` fails because `image` blocks are silently dropped (empty `Images`, and today's code still produces `Content` correctly from the text block, so check the `Images` assertion specifically fails). `TestDecodeImageOnlyMessageNotDropped` fails with "want 1 message, got 0" — the exact silent-drop bug this task exists to fix.

- [ ] **Step 3: Implement**

In `internal/anthropic/codec.go`, `convertMessage`'s `switch blk.Type` block currently reads:

```go
		switch blk.Type {
		case "text":
			texts = append(texts, blk.Text)
		case "tool_use":
```

Add an `image` case. First, add a `Source` field to `contentBlockWire` (currently lacks any image-related field) — change:

```go
type contentBlockWire struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// tool_result
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
}
```

to:

```go
type contentBlockWire struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// tool_result
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	// image (source.type == "base64" only — see design spec's Out of scope)
	Source *struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
	} `json:"source,omitempty"`
}
```

Then change the switch:

```go
		switch blk.Type {
		case "text":
			texts = append(texts, blk.Text)
		case "image":
			if blk.Source != nil && blk.Source.Type == "base64" {
				base.Images = append(base.Images, llm.Image{
					URL: "data:" + blk.Source.MediaType + ";base64," + blk.Source.Data,
				})
			}
		case "tool_use":
```

Finally, fix the drop condition — currently:

```go
	var out []llm.Message
	if base.Content != "" || len(base.ToolCalls) > 0 {
		out = append(out, base)
	}
	return append(out, toolResults...)
```

becomes:

```go
	var out []llm.Message
	if base.Content != "" || len(base.ToolCalls) > 0 || len(base.Images) > 0 {
		out = append(out, base)
	}
	return append(out, toolResults...)
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/anthropic/... -v`
Expected: PASS — all new tests plus every pre-existing test in this file (text, tool_use, tool_result, system, streaming) unaffected.

- [ ] **Step 5: Run the full build**

Run: `gofmt -l . && go build ./... && go vet ./...`
Expected: clean.

- [ ] **Step 6: Commit**

```bash
git add internal/anthropic/codec.go internal/anthropic/codec_test.go
git commit -m "feat(anthropic): parse image content blocks instead of silently dropping them"
```

---

### Task 4: Mock echo + DLP/capture invisibility regression tests

**Files:**
- Modify: `internal/providers/mock.go`
- Test: `internal/providers/mock_test.go` (extend)
- Test: `internal/httpapi/dlp_guardrails_test.go` (extend)
- Test: `internal/httpapi/capture_enqueue_test.go` (extend)

**Interfaces:**
- Consumes: `llm.Image`, `Message.Images` (Task 1)

- [ ] **Step 1: Write the failing test for Mock's echo extension**

Add to `internal/providers/mock_test.go` (same file, same imports already present):

```go
func TestMockChatEchoesImageCount(t *testing.T) {
	m := NewMock("mock")
	req := llm.ChatRequest{
		Model: "mock-gpt",
		Messages: []llm.Message{{
			Role: "user", Content: "what is this",
			Images: []llm.Image{{URL: "data:image/png;base64,AAAA"}, {URL: "data:image/png;base64,BBBB"}},
		}},
	}
	resp, err := m.Chat(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	content := resp.Choices[0].Message.Content
	if !strings.Contains(content, "2 image(s) attached") {
		t.Errorf("expected image count echoed, got %q", content)
	}
}

func TestMockChatNoImagesUnaffected(t *testing.T) {
	m := NewMock("mock")
	resp, err := m.Chat(context.Background(), userReq("plain text, no images"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(resp.Choices[0].Message.Content, "image") {
		t.Errorf("a request with no images must not mention images: %q", resp.Choices[0].Message.Content)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/providers/... -run TestMockChatEchoesImageCount -v`
Expected: FAIL — `Mock.content` doesn't look at `Images` yet, so the echoed string never mentions the image count.

- [ ] **Step 3: Implement Mock's echo extension**

In `internal/providers/mock.go`, `content` currently reads:

```go
func (m *Mock) content(req llm.ChatRequest) string {
	return fmt.Sprintf(
		"Mock response from provider %q (model %q). You said: %s",
		m.name, req.Model, lastUserText(req.Messages))
}
```

becomes:

```go
func (m *Mock) content(req llm.ChatRequest) string {
	base := fmt.Sprintf(
		"Mock response from provider %q (model %q). You said: %s",
		m.name, req.Model, lastUserText(req.Messages))
	if n := lastUserImageCount(req.Messages); n > 0 {
		base += fmt.Sprintf(" (with %d image(s) attached)", n)
	}
	return base
}
```

Add `lastUserImageCount` right after the existing `lastUserText` function:

```go
func lastUserImageCount(msgs []llm.Message) int {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			return len(msgs[i].Images)
		}
	}
	return 0
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/providers/... -run TestMockChat -v`
Expected: PASS, including the pre-existing `TestMockChatContent`/`TestMockToolCallTrigger`/etc. (unaffected — none of their requests carry `Images`).

- [ ] **Step 5: Write the DLP-invisibility test**

Add to `internal/httpapi/dlp_guardrails_test.go` (same file — it already imports what's needed: `context`, `testing`, `llm`, and has `newDLPEnforceTestServer`):

```go
// TestDlpEnforceIgnoresImages proves Images is invisible to DLP scanning
// by construction: a message with a planted secret in Content and a
// realistic-looking data URI in Images must produce byte-identical
// dlpEnforce results to the same message with Images unset.
func TestDlpEnforceIgnoresImages(t *testing.T) {
	s := newDLPEnforceTestServer(t, "http://unused.invalid")
	secret := "sk-ant-api03-aaaabbbbccccddddeeee1234"

	withoutImages := llm.ChatRequest{Model: "m", Messages: []llm.Message{{Role: "user", Content: "key: " + secret}}}
	withImages := llm.ChatRequest{Model: "m", Messages: []llm.Message{{
		Role: "user", Content: "key: " + secret,
		Images: []llm.Image{{URL: "data:image/png;base64,verylongfakeimagedatathatlookslikea/realimage=="}},
	}}}

	blocked1, msg1, res1 := s.dlpEnforce(context.Background(), authedKey{}, "openai", &withoutImages, false)
	blocked2, msg2, res2 := s.dlpEnforce(context.Background(), authedKey{}, "openai", &withImages, false)

	if blocked1 != blocked2 || msg1 != msg2 {
		t.Errorf("blocked/message differ: (%v,%q) vs (%v,%q)", blocked1, msg1, blocked2, msg2)
	}
	if len(res1.Findings) != len(res2.Findings) {
		t.Errorf("finding count differs: %d vs %d — Images must not affect scanning", len(res1.Findings), len(res2.Findings))
	}
}
```

- [ ] **Step 6: Write the capture-no-leak test**

Add to `internal/httpapi/capture_enqueue_test.go` (same file — already imports `bytes`, `testing`, `dlp`, `llm`):

```go
// TestCaptureBodyNeverIncludesImages proves the persistence guarantee:
// a message carrying Images must never leak the image URL/data into the
// serialized capture body, regardless of DLP redaction settings.
func TestCaptureBodyNeverIncludesImages(t *testing.T) {
	imageMarker := "totally-unique-fake-image-payload-marker-xyz123"
	msgs := []llm.Message{{
		Role: "user", Content: "describe this",
		Images: []llm.Image{{URL: "data:image/png;base64," + imageMarker}},
	}}

	body := captureBody(msgs, "a description", false, nil)

	if bytes.Contains(body, []byte(imageMarker)) {
		t.Errorf("captured body must never contain image data, got: %s", body)
	}
}
```

- [ ] **Step 7: Run both new httpapi tests**

Run: `go test ./internal/httpapi/... -run 'TestDlpEnforceIgnoresImages|TestCaptureBodyNeverIncludesImages' -v`
Expected: PASS — both should already pass without any `internal/httpapi` code change, since `dlp.go` and `capture_enqueue.go` are untouched and `Images`' `json:"-"` tag (Task 1) already makes this true by construction. If either fails, that's a real gap in Task 1's implementation — stop and report rather than adding httpapi-side code to route around it, since the design's whole point is that no such code should be needed.

- [ ] **Step 8: Run the full test suite**

Run: `gofmt -l . && go build ./... && go vet ./... && go test ./...`
Expected: all green.

- [ ] **Step 9: Commit**

```bash
git add internal/providers/mock.go internal/providers/mock_test.go internal/httpapi/dlp_guardrails_test.go internal/httpapi/capture_enqueue_test.go
git commit -m "feat(providers): Mock echoes image count; regression tests proving DLP/capture never see Images"
```

---

### Task 5: Live verification (controller)

- [ ] Rebuild the dev app (`docker compose -f deploy/docker-compose.yml up --build -d app`).
- [ ] Send the bug report's own exact JSON body to `POST /v1/chat/completions` against a `Mock`-backed alias (e.g. `mock-gpt`) with a real dev API key — confirm `200`, not the original `json: cannot unmarshal array...` error, and confirm the response content mentions "1 image(s) attached".
- [ ] Send the same style of request via `POST /v1/messages` (Anthropic protocol) with an Anthropic-shaped `image` content block against the same alias — confirm `200` and the image is reflected in the mock response, proving the Anthropic ingress fix works end to end too (not just the originally-reported OpenAI path).
- [ ] Send a plain-string-content request (no images) through the same alias on both `/v1/chat/completions` and `/v1/messages` — confirm both are byte-for-byte unaffected (regression check for the overwhelming majority of existing traffic).
- [ ] If a capture pipeline is enabled in dev (`capture.Enabled`), send an image request through it and inspect the actual stored capture row/blob — confirm no image data appears anywhere in it.
- [ ] Full existing e2e regression (chat + tool-calls, non-vision) still green — this feature only additively touches `llm.Message` (new field + new UnmarshalJSON), `internal/openai/client.go`'s encode path, and `internal/anthropic/codec.go`'s block switch; nothing else in the request path changes.
