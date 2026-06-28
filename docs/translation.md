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
stream flag, usage (prompt/completion tokens), finish/stop reason, and
streaming deltas (role, content, tool-call, finish, usage).

## Cross-protocol caveats

Translation is lossy for provider-specific features that have no IR
equivalent. When a request crosses protocols, these may degrade:

- **Anthropic prompt caching** (`cache_control`) — not represented in the
  OpenAI-shaped IR.
- **OpenAI `logprobs`, `n>1`, `seed`, `response_format`** — not mapped.
- **Reasoning controls** (`reasoning_effort`, extended thinking) — not mapped.
- **System prompt fidelity** — Anthropic `system` becomes a leading system
  message; structured system blocks are flattened to text.
- **Tool result shaping** — Anthropic `tool_result` blocks become IR `tool`
  messages; rich block content is flattened to text.

To avoid these losses, the router prefers a target whose `upstream_protocol`
matches the ingress protocol when the alias offers more than one. Operators
should keep same-protocol targets first in an alias's priority order when
fidelity matters.
