# Fallback-Worthy Errors — Design

**Date:** 2026-09-01
**Status:** approved

## Problem

`runChat`/`runStream` (`internal/httpapi/exec.go`) walk an alias's priority
tiers, trying each target in order. Today the decision to keep walking or
give up is driven by a single field, `providers.Error.Retryable`
(`internal/providers/errors.go`): `true` for upstream 429/5xx, `false` for
everything else. A non-retryable error aborts the whole walk immediately —
no lower-priority tier is ever tried, even when it would very likely have
succeeded.

This has caused two separate production incidents on the same mechanism:

1. The `text-flash` alias was pinned to a Groq model id the vendor had
   since removed. Every request got a `404 model_not_found` from that one
   tier and failed outright — the alias had no other tiers configured, but
   the underlying bug (no fallback on 4xx) is the same one below.
2. The `text` alias's tier-0 (OpenRouter free) is permanently `429`
   (shared free-tier pool exhausted platform-wide, not fixable here), so
   in practice every request lands on tier 1 (local Ollama gemma,
   `n_ctx=8192`). A long conversation history plus ~11 tool schemas
   (~94k tokens) exceeds that context window; Ollama/llama.cpp returns
   `400 exceed_context_size_error`. Tier 2 (`groq-pers`, a much larger
   context model) is never tried, even though it would likely have
   succeeded.

Both are the same root cause: `Retryable` conflates two different
questions — "might the exact same request succeed against the exact same
target if retried" (true meaning of 429/5xx) — with "is this target simply
wrong for this request, so a *different* target might do better." The
fix targets the second question specifically, for a short, known list of
error shapes — not a blanket "retry everything" change (that would risk
masking genuinely malformed client requests behind a confusing
multi-tier retry before an equally confusing final error).

## Architecture

**`providers.Error` gains a `Code string` field** (`internal/providers/errors.go`),
empty by default. Two recognized values, defined as constants:

```go
const (
	ErrCodeContextLengthExceeded = "context_length_exceeded"
	ErrCodeModelNotFound         = "model_not_found"
)
```

`Retryable` is unchanged in meaning and unchanged in every existing call
site that only cares about it (5xx/429 handling stays exactly as-is).
`Code` is purely additive.

**`httpError()` (`internal/providers/openai_compat.go`) gains best-effort
body parsing**, tried in order, first match wins, no match leaves `Code`
empty (fail-safe — a body that doesn't parse, or doesn't match, is
treated exactly like today):

- **OpenAI-style** (used by OpenAI itself, and by Groq/xAI/OpenRouter
  since they mirror the OpenAI error contract): unmarshal
  `{"error": {"type": string, "code": string}}` and map `error.code`
  directly — `"context_length_exceeded"` and `"model_not_found"` are
  OpenAI's own documented values, used verbatim.
- **llama.cpp/Ollama-style**: unmarshal
  `{"error": {"type": string, "message": string, "n_ctx": int,
  "n_prompt_tokens": int}}` and map `error.type ==
  "exceed_context_size_error"` to `ErrCodeContextLengthExceeded`.
  (llama.cpp/Ollama's own "model not found" error shape is not covered —
  no production incident has hit it through this path yet; the OpenAI-style
  parser already covers the Groq incident that did happen. Add a case
  if/when it's actually needed, per the "known cases only" scope decided
  for this design — no speculative vendor-shape coverage.)

If the OpenAI-shaped unmarshal fails outright (e.g. `code` arrives as
JSON `null`, which OpenAI does send for many *other* error kinds), the
attempt is simply discarded and the llama.cpp-shaped attempt is tried
next; if that also fails, `Code` stays empty. This is deliberately not
hardened against every possible JSON shape a vendor could send — the two
known codes we're targeting always arrive as non-null strings in
practice, so a stricter implementation would add complexity without
reducing risk for the cases this design actually needs to catch.

**New `providers.IsFallbackWorthy(err error) bool`** (`errors.go`,
alongside the existing `IsRetryable`):

```go
func IsFallbackWorthy(err error) bool {
	if IsRetryable(err) {
		return true
	}
	var pe *Error
	if errors.As(err, &pe) {
		switch pe.Code {
		case ErrCodeContextLengthExceeded, ErrCodeModelNotFound:
			return true
		}
	}
	return false
}
```

**`runChat` and `runStream`** (`internal/httpapi/exec.go`) each have one
call site that currently reads:

```go
if !providers.IsRetryable(callErr) {
    return ... // abort, no more tiers tried
}
```

Both become `if !providers.IsFallbackWorthy(callErr) { ... }` — the only
change to either function. The rest of the walk/backoff/busy-retry
machinery is untouched.

**Audio handlers (`internal/httpapi/api_audio.go`) are explicitly out of
scope** — they have their own `IsRetryable`-gated fallback loop, but no
production incident has occurred there, and extending this fix to audio
would be speculative scope creep beyond what's been observed.

**`classifyUpstreamErr`** (`exec.go`) gains one new branch, checked before
the existing `errAllBusy`/default fallback: if the final error (the one
propagated after every tier has been exhausted) is a `*providers.Error`
whose `Code` is `ErrCodeContextLengthExceeded` or `ErrCodeModelNotFound`,
the client gets `400 invalid_request_error` instead of the current
generic `502 upstream_error`. The message text is unchanged (it already
carries the real upstream response text via `Error()`) — only the
HTTP status and `type` field change, so the client sees an honest,
actionable 400 ("your request doesn't fit any configured backend") when
literally every tier has the same problem, instead of a generic gateway
error.

## Data flow example (incident 2, after the fix)

1. Client sends a 94k-token request to alias `text`.
2. Tier 0 (OpenRouter): `429` → `IsRetryable` true (unchanged) → next tier.
3. Tier 1 (Ollama): `400 exceed_context_size_error` → `httpError` parses
   the llama.cpp shape, sets `Code = ErrCodeContextLengthExceeded` →
   `IsFallbackWorthy` true (new) → next tier.
4. Tier 2 (`groq-pers`, large context): succeeds → response returned
   normally. (If tier 2 *also* failed with the same code, `classifyUpstreamErr`
   would map the final error to `400 invalid_request_error` instead of
   `502`.)

## Testing

- **`internal/providers`** (new test file or extend `errors_test.go` if
  one exists): unit tests calling `httpError` directly with raw
  OpenAI-shaped and llama.cpp-shaped byte literals — confirms `Code` is
  set correctly for each known case, confirms an unrecognized/malformed
  body leaves `Code` empty (`Retryable` unaffected either way). Unit
  tests for `IsFallbackWorthy`: retryable-without-code → true;
  non-retryable-with-known-code → true; non-retryable-with-empty-code →
  false; a plain non-`*Error` error → false (matches `IsRetryable`'s
  existing fallback behavior).
- **`internal/providers/mock.go`**: `Mock.Chat` gains one new trigger,
  alongside the existing `"fail"` substring check — a model name
  containing `"ctxfail"` returns
  `&Error{Status: 400, Retryable: false, Code: ErrCodeContextLengthExceeded,
  Message: "mock upstream context-length error for model " + req.Model}`.
  This lets a plan with two tiers (one `*-ctxfail`, one normal) exercise
  the new fallback path end-to-end through `runChat`/`runStream` without
  any real HTTP involved, mirroring exactly how the existing `"fail"`
  trigger already tests the 429/5xx retryable path.
- **`internal/httpapi`**: no full-stack handler test (matches this
  codebase's existing precedent — chat/messages handlers have no HTTP-level
  tests of their own), but `runChat`/`runStream` themselves ARE directly,
  cheaply unit-testable without a DB or real HTTP: `*Server{}` can be
  built as a bare struct literal with `regPtr` set to a `providers.Registry`
  holding two `Mock` instances and `router` set to
  `routing.NewRouter(nil)` — exactly the pattern `dlp_guardrails_test.go`'s
  `newDLPScanTextTestServer` already uses for narrow `*Server` construction.
  Confirmed feasible by reading `routing.Plan`/`Target` (plain structs,
  no DB needed to build one by hand) and `Router.NextRR` (only exercises
  `st` when a tier has >1 target — irrelevant for single-target tiers).
  New test file `internal/httpapi/exec_test.go`: a `newRunChatTestServer(t)`
  helper, then `TestRunChatFallsBackOnContextLengthExceeded` and
  `TestRunStreamFallsBackOnContextLengthExceeded` — a two-tier
  `routing.Plan` (`mock-ctxfail` → `mock-ok`), asserting the response
  comes from the second tier's provider, not an error.

## Out of scope

- Any vendor error shape beyond OpenAI-style and llama.cpp/Ollama-style
  (no incident has surfaced a third shape).
- Extending fallback-worthy classification to audio's `Transcriber`/
  `Synthesizer` call sites — no incident there.
- A blanket "all non-429 4xx fall back" policy — explicitly rejected in
  favor of the known-cases list, to avoid masking genuinely malformed
  client requests behind confusing multi-tier retries.
- Retrying the *same* target (this codebase has no such mechanism today,
  and this design doesn't add one — "fallback-worthy" only ever means
  "try the next tier," matching `Retryable`'s existing practical effect).
- Changing `busyRetries`/`busyBackoff` semantics (capacity-exhaustion
  retry is a separate, untouched mechanism).
