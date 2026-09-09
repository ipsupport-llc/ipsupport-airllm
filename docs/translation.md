# Protocol translation

The gateway accepts two ingress protocols (OpenAI chat-completions and
Anthropic Messages) and routes to upstream providers that natively speak one
of them. Translation is achieved through a single provider-neutral
intermediate representation (IR), `internal/llm`.

## How it works

```
client request ──decode──▶  llm IR  ──▶ provider (Chat / ChatStream)
                                            │
client response ◀─encode──  llm IR  ◀──────┘
```

- **Ingress decode:** `internal/openai` and `internal/anthropic` decode the
  client request into `llm.ChatRequest`.
- **Egress encode:** the same packages encode `llm.ChatResponse` /
  `llm.StreamChunk` back into the client's format (including SSE).
- **Providers** operate only on the IR, so any ingress can target any
  provider. When the client protocol equals the upstream protocol, a real
  provider can choose byte-passthrough for maximum fidelity (a real-provider
  optimization; the mock always goes through the IR).

This keeps the translation matrix O(protocols) instead of
O(protocols squared): each protocol needs only an IR codec, not a converter
to every other protocol.

## What the IR carries

Messages (role, content, name), tool definitions, tool calls and tool
results, tool_choice (passed through as raw JSON), temperature, max_tokens,
stream flag, usage (prompt/completion tokens, and the reasoning share of the
completion count — see
[API reference → Reasoning tokens](api.md#reasoning-tokens)), finish/stop
reason, and streaming deltas (role, content, tool-call, finish, usage).

## Cross-protocol caveats

Translation is lossy for provider-specific features that have no IR
equivalent. When a request crosses protocols, these may degrade:

- **Anthropic prompt caching** (`cache_control`) — not represented in the
  OpenAI-shaped IR.
- **OpenAI `logprobs`, `n>1`, `seed`, `response_format`** — not mapped.
- **Reasoning controls** (`reasoning_effort`, extended thinking) — not mapped.
  Reasoning *usage* is a separate matter and is not lossy: thinking tokens are
  counted, priced and capped on every path, and the Anthropic egress reports
  them inside `output_tokens`, which is where Anthropic itself puts them.
- **System prompt fidelity** — Anthropic `system` becomes a leading system
  message; structured system blocks are flattened to text.
- **Tool result shaping** — Anthropic `tool_result` blocks become IR `tool`
  messages; rich block content is flattened to text.

There's no way to avoid this today: every real provider kind
(openai/openrouter/xai/groq/ollama/vertex) speaks the OpenAI wire format
upstream regardless of which protocol the client used, so a request that
enters via Anthropic ingress always gets translated before it reaches any
of them. `alias_targets` records an `upstream_protocol` for audit purposes,
but it's derived from the provider's own kind — operators can't configure
it, and there is no protocol-aware target ordering to prefer.
