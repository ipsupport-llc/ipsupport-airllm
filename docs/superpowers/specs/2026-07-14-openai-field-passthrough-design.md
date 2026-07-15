# OpenAI Request-Field Passthrough — Design

**Date:** 2026-07-14
**Status:** approved

## Problem

The OpenAI ingress decodes requests into a fixed IR struct; any field not
explicitly mapped (`top_p`, `stop`, `frequency_penalty`, `presence_penalty`,
`seed`, `user`, `response_format`, `max_completion_tokens`,
`reasoning_effort`, `logit_bias`, ...) silently vanishes before the upstream
call. This is the same silent-drop class that caused the v0.1.10 tool-call
incident (`parallel_tool_calls` was one of these fields). A gateway should be
transparent: clients tune generation parameters and get JSON mode via
`response_format`; dropping them changes model behavior invisibly.

## Design

Generic passthrough instead of chasing fields one by one:

- `llm.ChatRequest` gains `Extra map[string]json.RawMessage` — unmapped
  OpenAI request fields, carried verbatim.
- `openai.DecodeChatRequest` decodes the body twice: into the wire struct
  (as today) and into a raw `map[string]json.RawMessage`. Keys the gateway
  owns are deleted from the map; the remainder becomes `Extra`. Owned keys:
  `model`, `messages`, `tools`, `tool_choice`, `temperature`, `max_tokens`,
  `stream`, `parallel_tool_calls`, `stream_options` (the gateway imposes its
  own on streams), `n`.
- **`n` guard:** the gateway's single-choice stream egress cannot represent
  multi-choice responses faithfully, so `n` present and ≠ 1 → 400
  `invalid_request_error` ("n is not supported") instead of silent
  corruption.
- `openai.EncodeChatRequest` merges `Extra` into the upstream body after
  marshalling the known struct (collisions impossible — owned keys were
  removed at decode).
- Unknown params now reach the upstream verbatim; if a strict upstream
  rejects one, the error surfaces to the client instead of the parameter
  silently disappearing — that is the correct transparency trade.
- Anthropic ingress unchanged (`Extra` stays nil): its parameters are named
  differently and still map explicitly; translating them is a separate
  backlog item.
- Model substitution unaffected: `Model` stays a struct field; `Extra` can
  never contain `model`.

## Testing

- Unit (`internal/openai`): decode splits owned vs extra keys; encode merges
  extras byte-verbatim (`top_p`, `stop` array, `response_format` object,
  `max_completion_tokens`); `n:1` allowed, `n:2` → error sentinel the
  handler maps to 400; round-trip request with no extras is byte-identical
  to today's encoding.
- Live (compose stub): send `top_p`/`stop`/`response_format`/
  `max_completion_tokens` through the gateway, assert `/last-request` shows
  them verbatim; `n:2` → 400.

## Out of scope

- Anthropic-ingress parameter translation (`top_k`, `stop_sequences`,
  `metadata`, `disable_parallel_tool_use`).
- Response-side extras (e.g. `logprobs` in choices) — request-side only.
