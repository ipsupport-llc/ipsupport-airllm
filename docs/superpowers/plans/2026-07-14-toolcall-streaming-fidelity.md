# Tool-Call Streaming Fidelity + Version Surfacing Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Streamed tool calls survive the gateway byte-faithfully — parallel calls keep their indexes, object-form arguments are tolerated instead of dropped, `parallel_tool_calls` reaches the upstream — and the console shows the running version.

**Architecture:** `llm.StreamChunk` carries indexed `ToolCallDelta`s; the OpenAI codec preserves the upstream index both ways; `llm.FunctionCall` gains a tolerant `UnmarshalJSON`; the Anthropic `StreamWriter` gets real content-block management; a build-stamped `internal/version` is exposed via `/api/me` and the console footer.

**Tech Stack:** Go 1.26, vanilla JS. No new deps. No DB migration.

**Spec:** `docs/superpowers/specs/2026-07-14-toolcall-streaming-fidelity-design.md`

## Global Constraints

- English only; no new Go dependencies; no environment-specific values in the repo.
- `llm.ToolCall` (complete messages) is UNCHANGED; only `StreamChunk.ToolCalls` changes type to `[]ToolCallDelta`.
- `FunctionCall` marshalling is unchanged (arguments always a JSON string); only unmarshalling becomes tolerant.
- `ChatRequest.ParallelToolCalls` is `*bool`: absent stays absent upstream (`omitempty`), false/true pass through verbatim.
- Anthropic egress: never emit a `"{}"` filler `partial_json`; every delta in a chunk is processed, not just `[0]`.
- Chart `version`/`appVersion` → 0.1.10 in this branch (release guard).
- `gofmt -l .` clean before every commit; `go build ./... && go vet ./... && go test ./...` green.

---

### Task 1: Indexed stream deltas + tolerant arguments + OpenAI codec

**Files:**
- Modify: `internal/llm/types.go`
- Modify: `internal/openai/client.go` (`upstreamChatRequest`, `upstreamChunk`, `ParseStreamChunk`, `EncodeChatRequest`)
- Modify: `internal/openai/stream.go` (`MarshalStreamChunk`)
- Modify: `internal/openai/codec.go` (`chatRequestWire`, `DecodeChatRequest`)
- Modify: `internal/providers/mock.go` (`ChatStream` tool-call yield, ~line 99)
- Test: `internal/llm/types_test.go` (new), `internal/openai/stream_test.go` (extend)

**Interfaces:**
- Produces: `llm.ToolCallDelta{Index int; ID, Type string; Function FunctionCall}`; `llm.StreamChunk.ToolCalls []ToolCallDelta`; `llm.ChatRequest.ParallelToolCalls *bool`; `(*llm.FunctionCall).UnmarshalJSON`. Task 2 consumes `ToolCallDelta` in the Anthropic StreamWriter.

- [ ] **Step 1: Write the failing tests**

`internal/llm/types_test.go`:

```go
package llm

import (
	"encoding/json"
	"testing"
)

func TestFunctionCallUnmarshalTolerant(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want FunctionCall
	}{
		{"string args", `{"name":"file","arguments":"{\"a\":1}"}`, FunctionCall{Name: "file", Arguments: `{"a":1}`}},
		{"object args", `{"name":"file","arguments":{"a":1}}`, FunctionCall{Name: "file", Arguments: `{"a":1}`}},
		{"array args", `{"name":"file","arguments":[1,2]}`, FunctionCall{Name: "file", Arguments: `[1,2]`}},
		{"null args", `{"name":"file","arguments":null}`, FunctionCall{Name: "file"}},
		{"absent args", `{"name":"file"}`, FunctionCall{Name: "file"}},
	}
	for _, c := range cases {
		var got FunctionCall
		if err := json.Unmarshal([]byte(c.in), &got); err != nil {
			t.Fatalf("%s: unmarshal: %v", c.name, err)
		}
		if got != c.want {
			t.Errorf("%s: got %+v want %+v", c.name, got, c.want)
		}
	}
}

func TestFunctionCallMarshalStaysString(t *testing.T) {
	b, err := json.Marshal(FunctionCall{Name: "file", Arguments: `{"a":1}`})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"name":"file","arguments":"{\"a\":1}"}` {
		t.Errorf("got %s", b)
	}
}
```

Extend `internal/openai/stream_test.go` (match its existing style):

```go
func TestMarshalStreamChunkPreservesToolCallIndex(t *testing.T) {
	meta := StreamMeta{ID: "id1", Model: "m", Created: 1}
	b, err := MarshalStreamChunk(meta, llm.StreamChunk{ToolCalls: []llm.ToolCallDelta{{
		Index: 1, ID: "call_2", Type: "function",
		Function: llm.FunctionCall{Name: "file", Arguments: `{"b":2}`},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"index":1`) {
		t.Errorf("upstream index lost: %s", b)
	}
}

func TestParseStreamChunkKeepsIndexAndObjectArgs(t *testing.T) {
	// grok shape: two parallel calls, whole arguments per delta, indexes 0/1
	c1, err := ParseStreamChunk([]byte(`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_2","type":"function","function":{"name":"file","arguments":"{\"b\":2}"}}]},"finish_reason":null}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(c1.ToolCalls) != 1 || c1.ToolCalls[0].Index != 1 || c1.ToolCalls[0].Function.Arguments != `{"b":2}` {
		t.Errorf("got %+v", c1.ToolCalls)
	}
	// spec-violating upstream: arguments as a raw object must parse, not error
	c2, err := ParseStreamChunk([]byte(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"file","arguments":{"a":1}}}]},"finish_reason":null}]}`))
	if err != nil {
		t.Fatalf("object arguments must not error: %v", err)
	}
	if c2.ToolCalls[0].Function.Arguments != `{"a":1}` {
		t.Errorf("got %q", c2.ToolCalls[0].Function.Arguments)
	}
}

func TestEncodeChatRequestParallelToolCalls(t *testing.T) {
	f := false
	b, err := EncodeChatRequest(llm.ChatRequest{Model: "m", ParallelToolCalls: &f}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"parallel_tool_calls":false`) {
		t.Errorf("flag dropped: %s", b)
	}
	b2, _ := EncodeChatRequest(llm.ChatRequest{Model: "m"}, false)
	if strings.Contains(string(b2), "parallel_tool_calls") {
		t.Errorf("nil flag must be omitted: %s", b2)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/llm/ ./internal/openai/`
Expected: FAIL (`ToolCallDelta` undefined, tolerant unmarshal missing, index lost).

- [ ] **Step 3: Implement**

`internal/llm/types.go` — after `ToolCall`:

```go
// ToolCallDelta is one streamed increment of a tool call. Index identifies
// the call within the message (OpenAI accumulation semantics): fragments
// sharing an Index belong to one call and their Arguments concatenate.
type ToolCallDelta struct {
	Index    int          `json:"index"`
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"`
	Function FunctionCall `json:"function"`
}
```

`StreamChunk.ToolCalls` becomes `[]ToolCallDelta` (update the field comment: "incremental tool calls; Index per OpenAI accumulation semantics").

`ChatRequest` gains `ParallelToolCalls *bool` after `ToolChoice`.

`FunctionCall` tolerant unmarshal (same file):

```go
// UnmarshalJSON accepts arguments as the spec's JSON-encoded string, but
// also tolerates upstreams that send a raw object or array (kept as its
// JSON text) or null/absent (empty string).
func (f *FunctionCall) UnmarshalJSON(b []byte) error {
	var w struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	f.Name = w.Name
	args := string(w.Arguments)
	if len(w.Arguments) == 0 || args == "null" {
		f.Arguments = ""
		return nil
	}
	var s string
	if err := json.Unmarshal(w.Arguments, &s); err == nil {
		f.Arguments = s
		return nil
	}
	f.Arguments = args
	return nil
}
```

`internal/openai/client.go`:
- `upstreamChatRequest` gains `ParallelToolCalls *bool \`json:"parallel_tool_calls,omitempty"\`` after `ToolChoice`; `EncodeChatRequest` copies it from the IR.
- `upstreamChunk` delta field becomes `ToolCalls []llm.ToolCallDelta \`json:"tool_calls"\`` (the wire `index` now decodes into `Index`).

`internal/openai/stream.go` — `MarshalStreamChunk` tool-call loop:

```go
for _, tc := range c.ToolCalls {
	choice.Delta.ToolCalls = append(choice.Delta.ToolCalls, streamToolCall{
		Index:    tc.Index,
		ID:       tc.ID,
		Type:     tc.Type,
		Function: streamToolFunc{Name: tc.Function.Name, Arguments: tc.Function.Arguments},
	})
}
```

`internal/openai/codec.go` — `chatRequestWire` gains `ParallelToolCalls *bool \`json:"parallel_tool_calls,omitempty"\``; `DecodeChatRequest` copies it into the IR.

`internal/providers/mock.go` ~line 99:

```go
if err := yield(llm.StreamChunk{ToolCalls: []llm.ToolCallDelta{{
	Index: 0, ID: tc.ID, Type: tc.Type, Function: tc.Function,
}}}); err != nil {
```

NOTE: `internal/anthropic/stream.go` reads `c.ToolCalls[0].ID/.Function` — it still compiles against `ToolCallDelta` (same field names). Do not fix its logic here; that is Task 2.

- [ ] **Step 4: Run the full suite**

Run: `gofmt -l . && go build ./... && go vet ./... && go test ./...`
Expected: PASS, no formatting diffs.

- [ ] **Step 5: Commit**

```bash
git add internal/llm/ internal/openai/ internal/providers/mock.go
git commit -m "fix(stream): preserve tool-call indexes, tolerate object arguments, pass parallel_tool_calls through"
```

---

### Task 2: Anthropic egress content-block management

**Files:**
- Modify: `internal/anthropic/stream.go` (rewrite `StreamWriter` block state + `Chunk`)
- Test: `internal/anthropic/stream_test.go` (new)

**Interfaces:**
- Consumes: `llm.ToolCallDelta` from Task 1 (`Index`, `ID`, `Type`, `Function.Name`, `Function.Arguments`).
- Produces: unchanged public API — `NewStreamWriter(w, flush, id, model, inputTokens)`, `(*StreamWriter).Chunk(llm.StreamChunk) error`.

- [ ] **Step 1: Write the failing tests**

`internal/anthropic/stream_test.go`:

```go
package anthropic

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/llm"
)

// collect runs chunks through a StreamWriter and returns the SSE events as
// (name, decoded-payload) pairs.
func collect(t *testing.T, chunks []llm.StreamChunk) []struct {
	Name string
	Data map[string]any
} {
	t.Helper()
	var buf bytes.Buffer
	sw := NewStreamWriter(&buf, func() {}, "msg_t", "m", 1)
	for _, c := range chunks {
		if err := sw.Chunk(c); err != nil {
			t.Fatal(err)
		}
	}
	var out []struct {
		Name string
		Data map[string]any
	}
	for _, ev := range strings.Split(strings.TrimSpace(buf.String()), "\n\n") {
		lines := strings.SplitN(ev, "\n", 2)
		name := strings.TrimPrefix(lines[0], "event: ")
		var data map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(lines[1], "data: ")), &data); err != nil {
			t.Fatalf("bad event payload: %v", err)
		}
		out = append(out, struct {
			Name string
			Data map[string]any
		}{name, data})
	}
	return out
}

func tcd(index int, id, name, args string) llm.ToolCallDelta {
	return llm.ToolCallDelta{Index: index, ID: id, Type: "function",
		Function: llm.FunctionCall{Name: name, Arguments: args}}
}

func TestParallelToolCallsGetSeparateBlocks(t *testing.T) {
	events := collect(t, []llm.StreamChunk{
		{Role: "assistant"},
		{ToolCalls: []llm.ToolCallDelta{tcd(0, "call_1", "file", `{"a":1}`)}},
		{ToolCalls: []llm.ToolCallDelta{tcd(1, "call_2", "file", `{"b":2}`)}},
		{FinishReason: "tool_calls"},
		{Usage: &llm.Usage{CompletionTokens: 5}},
	})
	var starts []map[string]any
	for _, e := range events {
		if e.Name == "content_block_start" {
			starts = append(starts, e.Data)
		}
	}
	if len(starts) != 2 {
		t.Fatalf("want 2 tool_use blocks, got %d", len(starts))
	}
	cb0 := starts[0]["content_block"].(map[string]any)
	cb1 := starts[1]["content_block"].(map[string]any)
	if cb0["id"] != "call_1" || cb1["id"] != "call_2" {
		t.Errorf("block ids: %v / %v", cb0["id"], cb1["id"])
	}
	if starts[0]["index"] == starts[1]["index"] {
		t.Errorf("blocks share an index: %v", starts[0]["index"])
	}
}

func TestFragmentedCallConcatenatesInOneBlock(t *testing.T) {
	events := collect(t, []llm.StreamChunk{
		{Role: "assistant"},
		{ToolCalls: []llm.ToolCallDelta{tcd(0, "call_1", "file", "")}},
		{ToolCalls: []llm.ToolCallDelta{tcd(0, "", "", `{"a"`)}},
		{ToolCalls: []llm.ToolCallDelta{tcd(0, "", "", `:1}`)}},
		{FinishReason: "tool_calls"},
		{Usage: &llm.Usage{CompletionTokens: 5}},
	})
	starts, sum := 0, ""
	for _, e := range events {
		switch e.Name {
		case "content_block_start":
			starts++
		case "content_block_delta":
			sum += e.Data["delta"].(map[string]any)["partial_json"].(string)
		}
	}
	if starts != 1 {
		t.Fatalf("want 1 block, got %d", starts)
	}
	if sum != `{"a":1}` {
		t.Errorf("partial_json sum = %q (a {} filler corrupts fragmented streams)", sum)
	}
}

func TestTextThenToolClosesTextBlock(t *testing.T) {
	events := collect(t, []llm.StreamChunk{
		{Role: "assistant"},
		{Content: "thinking..."},
		{ToolCalls: []llm.ToolCallDelta{tcd(0, "call_1", "file", `{"a":1}`)}},
		{FinishReason: "tool_calls"},
		{Usage: &llm.Usage{CompletionTokens: 5}},
	})
	var names []string
	for _, e := range events {
		names = append(names, e.Name)
	}
	joined := strings.Join(names, ",")
	want := "message_start,content_block_start,content_block_delta,content_block_stop,content_block_start,content_block_delta,content_block_stop,message_delta,message_stop"
	if joined != want {
		t.Errorf("event order:\n got %s\nwant %s", joined, want)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/anthropic/`
Expected: FAIL (single shared block, `{}` filler, second call lost).

- [ ] **Step 3: Rewrite the block state**

Replace the `started/blockOpen/stop` fields and `Chunk` in `internal/anthropic/stream.go`:

```go
	started   bool
	blockKind string // "" | "text" | "tool"
	blockIdx  int    // client-facing index of the open block
	blockTool int    // upstream tool-call index the open tool block belongs to
	nextIdx   int    // next client-facing block index
	stop      string
```

```go
// Chunk consumes one IR stream chunk and emits the corresponding Anthropic
// event(s).
func (s *StreamWriter) Chunk(c llm.StreamChunk) error {
	if !s.started {
		if err := s.event("message_start", map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id":            s.id,
				"type":          "message",
				"role":          "assistant",
				"model":         s.model,
				"content":       []any{},
				"stop_reason":   nil,
				"stop_sequence": nil,
				"usage":         map[string]int{"input_tokens": s.inputTokens, "output_tokens": 0},
			},
		}); err != nil {
			return err
		}
		s.started = true
	}

	switch {
	case len(c.ToolCalls) > 0:
		for _, tc := range c.ToolCalls {
			if err := s.ensureToolBlock(tc); err != nil {
				return err
			}
			if tc.Function.Arguments == "" {
				continue // head chunk; a filler here would corrupt the JSON
			}
			if err := s.event("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": s.blockIdx,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": tc.Function.Arguments},
			}); err != nil {
				return err
			}
		}
		return nil

	case c.Content != "":
		if s.blockKind != "text" {
			if err := s.closeBlock(); err != nil {
				return err
			}
			if err := s.event("content_block_start", map[string]any{
				"type":          "content_block_start",
				"index":         s.nextIdx,
				"content_block": map[string]any{"type": "text", "text": ""},
			}); err != nil {
				return err
			}
			s.blockKind, s.blockIdx = "text", s.nextIdx
			s.nextIdx++
		}
		return s.event("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": s.blockIdx,
			"delta": map[string]any{"type": "text_delta", "text": c.Content},
		})

	case c.FinishReason != "":
		s.stop = StopReason(c.FinishReason)
		return s.closeBlock()

	case c.Usage != nil:
		if err := s.event("message_delta", map[string]any{
			"type":  "message_delta",
			"delta": map[string]any{"stop_reason": s.stop, "stop_sequence": nil},
			"usage": map[string]int{"output_tokens": c.Usage.CompletionTokens},
		}); err != nil {
			return err
		}
		return s.event("message_stop", map[string]any{"type": "message_stop"})
	}

	// Role-only chunk: message_start already handled above.
	return nil
}

// ensureToolBlock opens a tool_use block for this delta's upstream index,
// closing whatever block was open, unless it is already the open block.
func (s *StreamWriter) ensureToolBlock(tc llm.ToolCallDelta) error {
	if s.blockKind == "tool" && s.blockTool == tc.Index {
		return nil
	}
	if err := s.closeBlock(); err != nil {
		return err
	}
	if err := s.event("content_block_start", map[string]any{
		"type":  "content_block_start",
		"index": s.nextIdx,
		"content_block": map[string]any{
			"type": "tool_use", "id": tc.ID, "name": tc.Function.Name, "input": map[string]any{},
		},
	}); err != nil {
		return err
	}
	s.blockKind, s.blockIdx, s.blockTool = "tool", s.nextIdx, tc.Index
	s.nextIdx++
	return nil
}

// closeBlock emits content_block_stop for the open block, if any.
func (s *StreamWriter) closeBlock() error {
	if s.blockKind == "" {
		return nil
	}
	if err := s.event("content_block_stop", map[string]any{"type": "content_block_stop", "index": s.blockIdx}); err != nil {
		return err
	}
	s.blockKind = ""
	return nil
}
```

- [ ] **Step 4: Run the full suite**

Run: `gofmt -l . && go build ./... && go vet ./... && go test ./...`
Expected: PASS (including the pre-existing `internal/anthropic/codec_test.go`).

- [ ] **Step 5: Commit**

```bash
git add internal/anthropic/
git commit -m "fix(anthropic-egress): one content block per tool call; no {} filler; text blocks close before tool blocks"
```

---

### Task 3: Build-stamped version in /api/me + console footer + chart 0.1.10

**Files:**
- Create: `internal/version/version.go`
- Modify: `internal/httpapi/api_self.go` (`handleMe`, ~line 28)
- Modify: `deploy/Dockerfile` (build stage)
- Modify: `.github/workflows/release.yml` (images job)
- Modify: `web/static/app.js` (`renderShell` sidebar-foot, ~line 138)
- Modify: `deploy/helm/airllm/Chart.yaml` (version + appVersion → 0.1.10)

**Interfaces:**
- Produces: `version.Version` (string, default `"dev"`); `/api/me` response gains `"version"`.

- [ ] **Step 1: version package**

`internal/version/version.go`:

```go
// Package version carries the build-time version stamp.
package version

// Version is the release version, stamped at build time via
// -ldflags "-X .../internal/version.Version=x.y.z". "dev" otherwise.
var Version = "dev"
```

- [ ] **Step 2: expose in /api/me**

In `handleMe` add `"version": version.Version,` to the response map; import `github.com/ipsupport-llc/ipsupport-airllm/internal/version`.

- [ ] **Step 3: stamp in Docker build**

`deploy/Dockerfile` build stage:

```dockerfile
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-X github.com/ipsupport-llc/ipsupport-airllm/internal/version.Version=${VERSION}" \
    -o /out/ipsupport-airllm ./cmd/ipsupport-airllm
```

(`ARG VERSION` must appear after the `FROM golang…` line so it is in scope for the RUN.)

`.github/workflows/release.yml` — in the `images` job add a step before build-push:

```yaml
      - id: ver
        run: echo "version=${GITHUB_REF_NAME#v}" >> "$GITHUB_OUTPUT"
```

and in the `docker/build-push-action@v6` step:

```yaml
          build-args: |
            VERSION=${{ steps.ver.outputs.version }}
```

(The dlp-bert matrix entry ignores the unused ARG — harmless warning.)

- [ ] **Step 4: console footer**

In `renderShell`'s `sidebar-foot`, above the copyright div:

```js
          <div class="ver">AirLLM v${esc(me.version || "dev")}</div>
```

Style is optional; if `.ver` needs anything, match `.copyright`'s muted style in `web/static/app.css`.

- [ ] **Step 5: chart bump**

`deploy/helm/airllm/Chart.yaml`: `version: 0.1.10`, `appVersion: "0.1.10"`.

- [ ] **Step 6: Run + commit**

```bash
gofmt -l . && go build ./... && go vet ./... && go test ./...
node --check web/static/app.js && make helm-lint
git add internal/version/ internal/httpapi/api_self.go deploy/Dockerfile .github/workflows/release.yml web/static/app.js deploy/helm/airllm/Chart.yaml
git commit -m "feat: build-stamped version in /api/me and the console footer; chart 0.1.10"
```

---

### Task 4: Live verification (controller)

- [ ] Rebuild the dev app; run the stub matrix (`tc-frag`, `tc-whole`, `tc-objargs`, `tc-multi` + new `pwhole` stub variant = grok shape) through `/v1/chat/completions`: accumulate client-side by index; every variant yields intact calls (objargs included, distinct indexes for multi/pwhole).
- [ ] `/v1/messages` (Anthropic ingress) against `tc-multi`: two tool_use blocks, valid JSON inputs.
- [ ] Operator re-runs the original failing coding-agent task against dev with the real xAI provider (`DEBUG_UPSTREAM_SSE=1` still on): arguments intact on every step.
- [ ] Playwright: full e2e regression + footer shows the version.
- [ ] Ledger sanity: usage rows still record tokens/cost for streamed tool-call responses.
