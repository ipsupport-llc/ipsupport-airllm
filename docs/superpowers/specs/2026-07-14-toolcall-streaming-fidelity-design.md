# Tool-Call Streaming Fidelity + Version Surfacing — Design

**Date:** 2026-07-14
**Status:** approved

## Problem

A coding agent (OpenAI client, streaming) run through the gateway gets
corrupted tool calls: the first call of a dialog arrives intact, later ones
arrive with the tool name but empty `{}` arguments, and the agent loops.
The same client pointed directly at the upstream works. Reproduced live
(dev gateway + real xAI provider, `DEBUG_UPSTREAM_SSE=1` tap) and with a
stub format matrix. Three gateway defects, all in stream conversion:

1. **Tool-call index is destroyed.** Captured ground truth: grok-4.5 emits
   *parallel* tool calls as separate SSE deltas carrying `index: 0` and
   `index: 1`, each with the complete arguments string. `llm.StreamChunk`
   carries `[]llm.ToolCall`, which has no index field, so
   `openai.ParseStreamChunk` drops the upstream index and
   `openai.MarshalStreamChunk` re-emits the position of the call *within
   that chunk* — always 0. OpenAI clients accumulate streamed tool calls
   by index, so both calls' argument strings concatenate under index 0
   into invalid JSON; tolerant clients surface `{}`. Single-call responses
   (a dialog's first step) survive, which is exactly the reported
   "step 1 OK, later steps empty" shape.
2. **`parallel_tool_calls` is silently dropped on ingress.** The IR has no
   such field, so a client's `"parallel_tool_calls": false` never reaches
   the upstream and the model keeps emitting parallel calls — feeding
   defect 1. Direct-to-upstream, the flag applies and calls stay single.
3. **A chunk whose `function.arguments` is a raw JSON object (spec
   violation some upstreams commit) is dropped whole** —
   `ParseStreamChunk` fails to unmarshal object-into-string and the caller
   skips the chunk silently. The client sees the name (from the head
   chunk) with empty arguments, or nothing at all.

The Anthropic egress (`anthropic.StreamWriter`) has the same class of
defects, worse: it reads only `ToolCalls[0]` per chunk, keeps a single
content block for the whole stream (a second parallel call's arguments
append to the first call's `input_json_delta` sequence; a tool call after
text appends into the *text* block), and pads empty arguments with a
literal `"{}"` partial_json that corrupts fragmented argument streams.

Verified NOT broken: ingress tool schema and multi-turn history round-trip
byte-for-byte (`required`/`enum` survive; assistant tool_calls echo and
tool results arrive intact upstream).

Also requested: the console should display the running version, so the
operator can tell what has rolled out (today the binary does not know its
version at all).

## Design

### 1. IR: stream deltas get an index (`internal/llm`)

`StreamChunk.ToolCalls` changes type from `[]ToolCall` to
`[]ToolCallDelta`:

```go
// ToolCallDelta is one streamed increment of a tool call. Index identifies
// the call within the message (OpenAI accumulation semantics): fragments
// sharing an Index belong to one call and their Arguments concatenate.
type ToolCallDelta struct {
	Index    int
	ID       string
	Type     string
	Function FunctionCall
}
```

`ToolCall` (used in complete `Message`s) is unchanged.

### 2. Tolerant arguments decoding (`internal/llm`)

`FunctionCall` gets an `UnmarshalJSON` that accepts `arguments` as a JSON
string (kept as-is), as `null`/absent (empty string), or as a raw
object/array (the raw JSON text becomes the string). Marshalling is
unchanged — always a string. This single hook makes the upstream stream
parse, the upstream non-stream decode, and the ingress echo all tolerant.

### 3. OpenAI codec fixes (`internal/openai`)

- `ParseStreamChunk`: decode wire `tool_calls` including `index` into
  `[]ToolCallDelta` (missing index → 0).
- `MarshalStreamChunk`: emit each delta's own `Index`, never the slice
  position.
- `EncodeChatRequest` / ingress `DecodeChatRequest`: carry a new
  `ChatRequest.ParallelToolCalls *bool` through verbatim (omitted when
  nil). No other request fields change.

### 4. Anthropic egress block management (`internal/anthropic`)

`StreamWriter` tracks the open block: kind (text | tool) + the upstream
tool index it belongs to + the client-facing block index counter.

- Text delta: if the open block is not text → close it, `content_block_start`
  (type text, next index).
- Tool delta with upstream index i: if the open block is not tool-i →
  close it, `content_block_start` (type tool_use, next index, id/name from
  this delta).
- Every delta in a chunk is processed (no more `ToolCalls[0]`).
- `partial_json` is emitted only for non-empty `Arguments` — no `"{}"`
  filler (an empty accumulated input is a valid empty object downstream).
- `content_block_stop` before the finish/message_delta, as today.

### 5. Version surfacing

- New `internal/version` package: `var Version = "dev"`, set at build time
  via `-ldflags -X`. `deploy/Dockerfile` gets `ARG VERSION=dev`;
  `.github/workflows/release.yml` passes the tag as a build arg.
- The `GET /api/me` response gains `"version"` (no new route needed — the
  console already fetches it after login).
- Console footer (next to the © line) shows `AirLLM v0.1.10` after login.

### 6. Diagnostics kept from the investigation

- `DEBUG_UPSTREAM_SSE=1` logs upstream request bodies and raw SSE lines
  (dev-only tool; off by default; prompts land in logs when enabled).
- A malformed upstream chunk now logs a WARN before being skipped — the
  silent drop hid defect 3.

## Testing

- Unit (`internal/llm`): tolerant `FunctionCall` unmarshal — string,
  object, array, null, absent.
- Unit (`internal/openai`): parse preserves upstream index; marshal emits
  it; a two-parallel-call stream (grok shape: whole call per chunk,
  indexes 0/1) round-trips with distinct indexes and intact argument
  strings; object-arguments chunk parses instead of erroring;
  `parallel_tool_calls` encodes/decodes.
- Unit (`internal/anthropic`): fragmented single call → one tool_use block
  whose partial_json concatenation equals the arguments; two parallel
  calls → two blocks with correct ids/names; text-then-tool → text block
  closed before the tool_use block opens; no `"{}"` filler emitted.
- Live (compose stub matrix): variants `frag`, `whole`, `objargs`,
  `multi` (fragmented, indexes 0/1), `pwhole` (grok shape) through
  `/v1/chat/completions` — client-side accumulation by index yields intact
  calls for all; `objargs` yields the call instead of dropping it.
- Live (operator): re-run the original failing coding-agent task against
  dev with the real xAI provider.
- Playwright: full e2e regression + version visible in the footer.

## Out of scope

- Passing through other unmapped OpenAI request fields (`top_p`, `stop`,
  `response_format`, ...) — backlog.
- Anthropic-ingress `disable_parallel_tool_use` mapping — backlog.
- A metric for malformed upstream chunks (WARN log only for now).
