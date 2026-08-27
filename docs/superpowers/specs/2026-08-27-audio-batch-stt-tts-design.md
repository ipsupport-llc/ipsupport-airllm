# Batch Audio (STT + TTS) — Design

**Date:** 2026-08-27
**Status:** approved

## Problem

The gateway routes text chat traffic only. The operator has a real client
that needs speech-to-text and text-to-speech, batch first (live/streaming
voice is a separate, later phase — out of scope here). The provider
landscape (researched earlier this session): OpenAI, Groq, and self-hosted
servers (speaches, LocalAI) all expose STT/TTS behind the same OpenAI-shaped
HTTP API (`/audio/transcriptions` multipart, `/audio/speech` JSON→binary),
so one provider-agnostic implementation covers all of them — no per-vendor
code, same pattern as chat's `OpenAICompat`.

## Architecture

**New package `internal/audio`** — provider-neutral IR for audio, mirroring
`internal/llm`'s role for chat but a distinct shape (multipart input,
binary output are nothing like chat's JSON):

```go
// TranscriptionRequest is a request to transcribe audio to text.
type TranscriptionRequest struct {
	Model    string
	Audio    []byte
	Filename string // some providers infer format from the extension
	Language string // optional ISO-639-1 hint
	Prompt   string // optional context/spelling guidance
}

// TranscriptionResponse is a transcription result.
type TranscriptionResponse struct {
	Text            string
	DurationSeconds float64 // upstream-reported audio duration, for pricing
}

// SpeechRequest is a request to synthesize speech from text.
type SpeechRequest struct {
	Model          string
	Input          string
	Voice          string
	ResponseFormat string // mp3/wav/opus/...; provider default if empty
}

// SpeechResponse is synthesized audio.
type SpeechResponse struct {
	Audio       []byte
	ContentType string // e.g. "audio/mpeg", from the upstream response
}
```

**Provider capabilities** (`internal/providers`), same pattern as
`PricedModelLister`:

```go
type Transcriber interface {
	Transcribe(ctx context.Context, req audio.TranscriptionRequest) (audio.TranscriptionResponse, error)
}

type Synthesizer interface {
	Synthesize(ctx context.Context, req audio.SpeechRequest) (audio.SpeechResponse, error)
}
```

`OpenAICompat` implements both:

- `Transcribe`: `POST {base}/audio/transcriptions`, `multipart/form-data`
  (`model`, `file` with `Filename`, optional `language`/`prompt`). Always
  requests `response_format=verbose_json` upstream regardless of what the
  gateway's own client asked for — the gateway needs `duration` for
  pricing. Non-2xx → the existing `httpError` path.
- `Synthesize`: `POST {base}/audio/speech`, JSON body (`model`, `input`,
  `voice`, `response_format`). Response body is read whole (batch, no
  streaming); `Content-Type` is read from the upstream response header.

**Routing**: no new schema. `routing.Resolve` is reused as-is — an audio
request's `model` field is an alias name, resolved through the same
`model_aliases`/`alias_targets` tables and the same tier/fallback
mechanics as chat. The audio handler type-asserts the resolved target's
provider implements `Transcriber`/`Synthesizer`; a target that doesn't
(e.g., an alias pointed at a text-only provider) fails with a clear 400
instead of a confusing upstream error.

**Ingress** (`internal/httpapi/api_audio.go`, new file):

- `POST /v1/audio/transcriptions` — decode `multipart/form-data` (`model`,
  `file` required; `language`, `prompt` optional — client-supplied
  `response_format` is accepted but ignored, v1 only returns the default
  `{"text": "..."}` shape). Resolve alias, run the fallback loop (reuse
  `runChat`-style tier iteration, but over `Transcriber` calls — no
  streaming variant needed for v1), DLP-scan the transcript (see below),
  price by duration, ledger + capture, respond `{"text": "..."}`.
- `POST /v1/audio/speech` — decode JSON (`model`, `input` required;
  `voice`, `response_format` optional). Resolve alias, DLP-scan the input
  text, run the fallback loop over `Synthesizer` calls, price by input
  character count, ledger + capture, respond with the raw audio bytes and
  the upstream's `Content-Type`.

Both handlers are policy-gated the same way chat is (`ak.Policy.Allows`,
key limits) before calling the provider.

## DLP (separate toggle from chat)

A new per-alias column, `dlp_audio_scan boolean NOT NULL DEFAULT true`
(same migration as pricing's `unit` column), independently gates whether
layer-1 deterministic DLP scanning runs on audio text for that alias —
independent from `dlp_model_scan` (which gates the layer-2 BERT scan for
*chat* aliases) and from the alias being usable for chat at all. When on,
the *global* DLP config's `enabled`/`action` still governs the actual
behavior (flag/redact/block), same relationship `dlp_model_scan` has to
the global model config.

A new small function, `dlpScanText(ctx, ak, ingress, text string) (blocked
bool, message string, findings []dlp.Finding)`, factors out the
layer-1-only subset of `dlpEnforce` (pattern scan, action handling, audit
recording) for a single string — audio has no message array, no
model-scan budget/scope, so reusing `dlpEnforce`'s full signature would
mean faking a `[]llm.Message` for no reason. `dlpEnforce` itself is
unchanged (chat keeps using it as-is).

- STT: scans the **transcribed text** (secrets/PII may have been spoken).
  `action=redact` returns the redacted text to the client; `action=block`
  returns an error instead of the transcript; `action=flag` returns the
  transcript unredacted but logs the incident (same semantics as chat).
- TTS: scans the **input text** (the thing being spoken), same actions.
  The output audio is never scanned — DLP governs prompts/input only, by
  design, same as chat's response text today.

## Capture

No new capture machinery. The existing `enqueueCapture`/`captureBody`
(shaped around `[]llm.Message` + a response string) already fits both
directions without modification:

- STT: `msgs = nil`, `response = transcript text` (no raw audio is ever
  captured or stored — voice biometric data, too sensitive for a default
  capture pipeline).
- TTS: `msgs = [{Role: "user", Content: input text}]`, `response = ""`
  (again, no raw audio stored).

## Pricing (real units, not a stub)

STT is priced per **duration of audio**, TTS per **character of input
text** — neither fits the existing token-based columns' *meaning*, but
both fit the existing *scale* (price per 1,000,000 of the unit): OpenAI
literally documents TTS as "$/1M characters" already. STT will use
**seconds** (not minutes) as the base unit for precision; a $0.006/minute
rate is entered as $10,000 per 1M seconds — an odd number to hand-type,
but this keeps one formula for every unit type, and manual entry is the
existing norm for any non-OpenRouter provider's prices already (audio
pricing is never auto-imported: Groq/OpenAI don't publish per-model
prices via `/models`, only `PricedModelLister`/OpenRouter does).

- Migration adds `pricing.unit text NOT NULL DEFAULT 'tokens' CHECK (unit
  IN ('tokens', 'audio_second', 'text_char'))`.
- `pricing.Price` gains `Unit string`; `pricing.Table.Load`/`Set` carry it
  through.
- Two new methods on `Table`, alongside the existing `CostMicroUSD`:

```go
// AudioCostMicroUSD prices a transcription by duration: seconds/1e6 *
// the priced row's InputPer1M (interpreted as $ per 1,000,000 seconds).
func (t *Table) AudioCostMicroUSD(provider, model string, seconds float64) int64

// TTSCostMicroUSD prices a synthesis by input character count: chars/1e6
// * the priced row's InputPer1M (interpreted as $ per 1,000,000 chars).
func (t *Table) TTSCostMicroUSD(provider, model string, chars int) int64
```

  Both share the exact-then-wildcard lookup `CostMicroUSD` already uses;
  only the quantity and which field they read differ. An unpriced
  provider+model costs 0 (unchanged behavior).
- Console pricing tab: a "Unit" column/selector on each price row (tokens
  default; audio_second / text_char selectable), so an operator can price
  an audio model without confusing it for a token-priced one.

## Usage limits (separate dimension, not folded into tokens/cost)

`internal/limits` hardcodes exactly two units today (`tok`, `cost`) with
dedicated Redis key helpers and dedicated check/add logic — it is
deliberately not generic. Extending it to a third+fourth unit follows the
same explicit pattern rather than generalizing already-tested code:

- `policy.Limits` gains `AudioSeconds map[string]int64` and `TTSChars
  map[string]int64` (same shape as `Tokens`: window name → cap).
- `limits.Limiter` gains `audioSecKey`/`ttsCharKey` Redis key builders and
  mirrors the existing `Check`/`Add` bucket logic for these two windows,
  alongside (not replacing) the tokens/cost checks.
- The audio handlers call `Check` before the provider call and `Add` after
  a successful one (STT adds `DurationSeconds`; TTS adds the input
  character count) — same check-before/increment-after shape as chat.
- No console change needed: the role editor already edits `limits` as a
  raw JSON textarea (`policy.Limits` round-trips through `json.RawMessage`
  end to end), and `fmtLimits` in `web/static/app.js` already renders any
  dimension key generically (`"${val} ${dim}/${win}"`). An operator can
  set `audio_seconds`/`tts_chars` caps today, the moment the Go side
  understands those keys — verified by reading the current UI code before
  writing this plan, not assumed.

## Testing

- Unit (`internal/audio`): none needed beyond what compiles — these are
  plain data structs.
- Unit (`internal/providers`): `Transcribe`/`Synthesize` against an
  httptest server — multipart body sent correctly, `verbose_json`
  requested, duration parsed; non-2xx → error; `Synthesize` request body
  and returned `Content-Type`/bytes round-trip.
- Unit (`internal/pricing`): `AudioCostMicroUSD`/`TTSCostMicroUSD` — exact
  match, wildcard fallback, unit mismatch safety (a `text_char`-priced row
  looked up by the STT path still uses whatever `InputPer1M` says — the
  caller picks the right method, the table doesn't cross-check unit; a
  test documents this so it's a known, not an accidental, behavior).
- Unit (`internal/limits`): audio_seconds/tts_chars Check/Add mirror the
  existing token tests (window math, fail-open on Redis error).
- Unit (`internal/httpapi`): `dlpScanText` — flag/redact/block actions on
  a bare string, matching `dlpEnforce`'s existing action-handling tests'
  shape.
- Handler tests (httptest + mock provider extended with
  `Transcribe`/`Synthesize`): both endpoints happy-path; alias resolves to
  a provider missing the capability → 400; DLP block → error, no provider
  call made; policy-denied model → 403.
- Live (compose): extend the scratchpad stub server (or wire a real
  `speaches` container) to serve `/audio/transcriptions` and
  `/audio/speech`; verify end-to-end through an alias — cost recorded
  with the right unit, DLP incident on a planted secret in a fake
  transcript, capture row has text but no audio bytes, limiter blocks
  after exceeding a tiny audio_seconds/tts_chars cap.
- Playwright: pricing tab shows the unit selector and accepts an
  audio-priced row; role editor shows the two new limit fields.

## Out of scope (this pass)

- Live/streaming voice (WebSocket, real-time STT or speech-to-speech) —
  separate future phase; xAI's Voice Agent API (speech-to-speech,
  launched Dec 2025, already have xAI configured as a provider) is the
  leading candidate when that phase starts.
- `response_format=verbose_json`/`srt`/`vtt` passthrough to the client on
  `/v1/audio/transcriptions` — v1 always returns `{"text": "..."}`.
- Auto-importing audio prices from any provider's catalog.
- Per-key audio-specific policy beyond the two new limit dimensions (e.g.
  a separate "allowed audio models" list) — the existing `allowed_models`
  list already covers it, since audio aliases are ordinary aliases.
