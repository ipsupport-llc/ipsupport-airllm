# Batch Audio (STT + TTS) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Batch speech-to-text (`POST /v1/audio/transcriptions`) and text-to-speech (`POST /v1/audio/speech`), OpenAI-shaped, provider-agnostic (any provider speaking the OpenAI audio API — Groq, OpenAI, local speaches/LocalAI).

**Architecture:** A new `internal/audio` IR package (multipart-in/binary-out, distinct from chat's `internal/llm`); two new provider capabilities (`Transcriber`, `Synthesizer`) implemented by the existing `OpenAICompat`; the existing alias/routing system is reused unchanged; pricing and per-key limits each grow one new unit dimension (duration, characters) alongside the existing token/cost ones; a small `dlpScanText` helper factors the layer-1-only subset of DLP out of `dlpEnforce` for single strings.

**Tech Stack:** Go 1.26, pgx v5, vanilla JS. No new dependencies (multipart handling is `net/http`/`mime/multipart`, both stdlib).

**Spec:** `docs/superpowers/specs/2026-08-27-audio-batch-stt-tts-design.md`

## Global Constraints

- English only; no new Go dependencies; no environment-specific values.
- No new alias/routing schema — audio aliases are ordinary `model_aliases` rows; the handler capability-asserts the resolved provider.
- STT prices by audio duration in **seconds** (not minutes); TTS prices by **input character count**. Both reuse the `X/1e6 * InputPer1M` shape already used for tokens — same formula, different quantity.
- `internal/limits` stays intentionally non-generic (two new named dimensions added the same explicit way as the existing two, not a generalized "any unit" rewrite).
- No raw audio bytes are ever captured, logged, or persisted anywhere (ledger, capture, DLP) — only text (transcript for STT, input for TTS) and numeric metadata (duration, char count, cost).
- `gofmt -l .` clean before every commit; `go build ./... && go vet ./... && go test ./...` green.

---

### Task 1: Audio IR + provider capabilities + Mock implementation

**Files:**
- Create: `internal/audio/audio.go`
- Modify: `internal/providers/provider.go` (capability interfaces, after `PricedModelLister`)
- Modify: `internal/providers/mock.go` (implement both, deterministic)
- Test: `internal/providers/mock_test.go` (extend)

**Interfaces:**
- Produces (consumed by Tasks 2 and 6):

```go
// internal/audio/audio.go
package audio

// TranscriptionRequest is a provider-neutral request to transcribe audio to text.
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

```go
// internal/providers/provider.go additions
type Transcriber interface {
	Transcribe(ctx context.Context, req audio.TranscriptionRequest) (audio.TranscriptionResponse, error)
}

type Synthesizer interface {
	Synthesize(ctx context.Context, req audio.SpeechRequest) (audio.SpeechResponse, error)
}
```

- [ ] **Step 1: Write the IR package**

Create `internal/audio/audio.go` with the exact four types shown above, plus this package doc comment at the top:

```go
// Package audio holds the provider-neutral intermediate representation for
// batch speech-to-text and text-to-speech requests. Unlike internal/llm
// (JSON in, JSON/SSE out), audio's wire shapes are multipart-in and
// binary-out, so it gets its own types rather than extending llm's.
package audio
```

- [ ] **Step 2: Add the capability interfaces**

In `internal/providers/provider.go`, add the import `"github.com/ipsupport-llc/ipsupport-airllm/internal/audio"` and the two interfaces above, placed after `PricedModelLister` (~line 45), each with a doc comment:

```go
// Transcriber is implemented by providers that can transcribe audio to
// text. Providers without a transcription capability simply do not
// implement it.
type Transcriber interface {
	Transcribe(ctx context.Context, req audio.TranscriptionRequest) (audio.TranscriptionResponse, error)
}

// Synthesizer is implemented by providers that can synthesize speech from
// text. Providers without a synthesis capability simply do not implement it.
type Synthesizer interface {
	Synthesize(ctx context.Context, req audio.SpeechRequest) (audio.SpeechResponse, error)
}
```

- [ ] **Step 3: Write the failing tests for Mock**

Add to `internal/providers/mock_test.go`:

```go
func TestMockTranscribe(t *testing.T) {
	m := NewMock("mock")
	resp, err := m.Transcribe(context.Background(), audio.TranscriptionRequest{
		Model: "mock-whisper", Audio: []byte("fake-audio-bytes"), Filename: "clip.wav",
	})
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if resp.Text == "" {
		t.Error("want non-empty transcript")
	}
	if resp.DurationSeconds <= 0 {
		t.Errorf("DurationSeconds = %v, want > 0", resp.DurationSeconds)
	}
}

func TestMockSynthesize(t *testing.T) {
	m := NewMock("mock")
	resp, err := m.Synthesize(context.Background(), audio.SpeechRequest{
		Model: "mock-tts", Input: "hello world", Voice: "default",
	})
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if len(resp.Audio) == 0 {
		t.Error("want non-empty audio bytes")
	}
	if resp.ContentType == "" {
		t.Error("want a non-empty ContentType")
	}
}
```

Add `"github.com/ipsupport-llc/ipsupport-airllm/internal/audio"` to that test file's imports.

- [ ] **Step 4: Run tests to verify they fail**

Run: `go test ./internal/providers/ -run TestMockTranscribe -v` and `-run TestMockSynthesize -v`
Expected: FAIL — `m.Transcribe`/`m.Synthesize` undefined (Mock doesn't implement the interfaces yet).

- [ ] **Step 5: Implement Mock's Transcribe/Synthesize**

In `internal/providers/mock.go`, add `"github.com/ipsupport-llc/ipsupport-airllm/internal/audio"` to imports, then append:

```go
// Transcribe returns a deterministic fake transcript, echoing the audio
// length as a stand-in duration (real providers report their own).
func (m *Mock) Transcribe(_ context.Context, req audio.TranscriptionRequest) (audio.TranscriptionResponse, error) {
	return audio.TranscriptionResponse{
		Text:            fmt.Sprintf("mock transcript of %q (%d bytes)", req.Filename, len(req.Audio)),
		DurationSeconds: float64(len(req.Audio)) / 16000, // arbitrary deterministic stand-in
	}, nil
}

// Synthesize returns deterministic fake "audio" (not real audio bytes —
// good enough for routing/pricing/limits tests, which never decode it).
func (m *Mock) Synthesize(_ context.Context, req audio.SpeechRequest) (audio.SpeechResponse, error) {
	return audio.SpeechResponse{
		Audio:       []byte("mock-audio:" + req.Voice + ":" + req.Input),
		ContentType: "audio/mpeg",
	}, nil
}
```

- [ ] **Step 6: Run the full suite**

Run: `gofmt -l . && go build ./... && go vet ./... && go test ./...`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/audio/ internal/providers/provider.go internal/providers/mock.go internal/providers/mock_test.go
git commit -m "feat(audio): IR types + Transcriber/Synthesizer capabilities + Mock impl"
```

---

### Task 2: OpenAICompat.Transcribe / Synthesize

**Files:**
- Modify: `internal/providers/openai_compat.go`
- Test: `internal/providers/openai_compat_transcribe_test.go` (new)

**Interfaces:**
- Consumes: `audio.TranscriptionRequest`/`Response`, `audio.SpeechRequest`/`Response` (Task 1).
- Produces: `(*OpenAICompat).Transcribe`, `(*OpenAICompat).Synthesize` — the concrete implementation Task 6's handlers call through the `Transcriber`/`Synthesizer` interfaces.

- [ ] **Step 1: Write the failing tests**

Create `internal/providers/openai_compat_transcribe_test.go`:

```go
package providers

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/audio"
)

func TestOpenAICompatTranscribe(t *testing.T) {
	var gotContentType, gotModel, gotFilename string
	var gotAudio []byte
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatalf("server: ParseMultipartForm: %v", err)
		}
		gotModel = r.FormValue("model")
		if rf := r.FormValue("response_format"); rf != "verbose_json" {
			t.Errorf("server: response_format = %q, want verbose_json (gateway must always request it upstream)", rf)
		}
		file, hdr, err := r.FormFile("file")
		if err != nil {
			t.Fatalf("server: FormFile: %v", err)
		}
		defer file.Close()
		gotFilename = hdr.Filename
		gotAudio, _ = io.ReadAll(file)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"text":"hello world","duration":1.5}`))
	}))
	defer ts.Close()

	p := NewOpenAICompat("up", "openai", ts.URL, "sk-test")
	resp, err := p.Transcribe(context.Background(), audio.TranscriptionRequest{
		Model: "whisper-1", Audio: []byte("fake-wav-bytes"), Filename: "clip.wav",
	})
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if resp.Text != "hello world" {
		t.Errorf("Text = %q, want %q", resp.Text, "hello world")
	}
	if resp.DurationSeconds != 1.5 {
		t.Errorf("DurationSeconds = %v, want 1.5", resp.DurationSeconds)
	}
	if gotModel != "whisper-1" {
		t.Errorf("upstream model field = %q, want whisper-1", gotModel)
	}
	if gotFilename != "clip.wav" {
		t.Errorf("upstream filename = %q, want clip.wav", gotFilename)
	}
	if string(gotAudio) != "fake-wav-bytes" {
		t.Errorf("upstream audio bytes = %q, want fake-wav-bytes", gotAudio)
	}
	if !strings.HasPrefix(gotContentType, "multipart/form-data") {
		t.Errorf("request Content-Type = %q, want multipart/form-data prefix", gotContentType)
	}
}

func TestOpenAICompatTranscribeNon200(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"bad file"}`, http.StatusBadRequest)
	}))
	defer ts.Close()

	p := NewOpenAICompat("up", "openai", ts.URL, "sk-test")
	_, err := p.Transcribe(context.Background(), audio.TranscriptionRequest{Model: "whisper-1", Audio: []byte("x"), Filename: "a.wav"})
	if err == nil {
		t.Fatal("want an error for a non-2xx upstream response")
	}
}

func TestOpenAICompatSynthesize(t *testing.T) {
	var gotBody []byte
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Write([]byte("fake-mp3-bytes"))
	}))
	defer ts.Close()

	p := NewOpenAICompat("up", "openai", ts.URL, "sk-test")
	resp, err := p.Synthesize(context.Background(), audio.SpeechRequest{
		Model: "tts-1", Input: "hello", Voice: "alloy",
	})
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if string(resp.Audio) != "fake-mp3-bytes" {
		t.Errorf("Audio = %q, want fake-mp3-bytes", resp.Audio)
	}
	if resp.ContentType != "audio/mpeg" {
		t.Errorf("ContentType = %q, want audio/mpeg", resp.ContentType)
	}
	if !strings.Contains(string(gotBody), `"model":"tts-1"`) || !strings.Contains(string(gotBody), `"input":"hello"`) || !strings.Contains(string(gotBody), `"voice":"alloy"`) {
		t.Errorf("upstream body missing expected fields: %s", gotBody)
	}
}

func TestOpenAICompatSynthesizeNon200(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"bad voice"}`, http.StatusBadRequest)
	}))
	defer ts.Close()

	p := NewOpenAICompat("up", "openai", ts.URL, "sk-test")
	_, err := p.Synthesize(context.Background(), audio.SpeechRequest{Model: "tts-1", Input: "hi", Voice: "nope"})
	if err == nil {
		t.Fatal("want an error for a non-2xx upstream response")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/providers/ -run "TestOpenAICompatTranscribe|TestOpenAICompatSynthesize" -v`
Expected: FAIL — `p.Transcribe`/`p.Synthesize` undefined.

- [ ] **Step 3: Implement**

In `internal/providers/openai_compat.go`, add imports `"mime/multipart"` and `"github.com/ipsupport-llc/ipsupport-airllm/internal/audio"`, then append:

```go
// Transcribe uploads audio for transcription. It always requests
// response_format=verbose_json upstream — regardless of what the
// gateway's own client asked for — because the gateway needs the
// reported duration for pricing.
func (p *OpenAICompat) Transcribe(ctx context.Context, in audio.TranscriptionRequest) (audio.TranscriptionResponse, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("model", in.Model); err != nil {
		return audio.TranscriptionResponse{}, err
	}
	if err := mw.WriteField("response_format", "verbose_json"); err != nil {
		return audio.TranscriptionResponse{}, err
	}
	if in.Language != "" {
		if err := mw.WriteField("language", in.Language); err != nil {
			return audio.TranscriptionResponse{}, err
		}
	}
	if in.Prompt != "" {
		if err := mw.WriteField("prompt", in.Prompt); err != nil {
			return audio.TranscriptionResponse{}, err
		}
	}
	fw, err := mw.CreateFormFile("file", in.Filename)
	if err != nil {
		return audio.TranscriptionResponse{}, err
	}
	if _, err := fw.Write(in.Audio); err != nil {
		return audio.TranscriptionResponse{}, err
	}
	if err := mw.Close(); err != nil {
		return audio.TranscriptionResponse{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/audio/transcriptions", &buf)
	if err != nil {
		return audio.TranscriptionResponse{}, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if p.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.apiKey)
	}

	resp, err := p.hc.Do(req)
	if err != nil {
		return audio.TranscriptionResponse{}, &Error{Status: http.StatusBadGateway, Retryable: true, Message: err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return audio.TranscriptionResponse{}, httpError(p.name, resp.StatusCode, b)
	}

	var w struct {
		Text     string  `json:"text"`
		Duration float64 `json:"duration"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&w); err != nil {
		return audio.TranscriptionResponse{}, err
	}
	return audio.TranscriptionResponse{Text: w.Text, DurationSeconds: w.Duration}, nil
}

// Synthesize requests text-to-speech audio. The response is read whole —
// batch only, no streaming — and the upstream Content-Type is passed
// through so the client gets the right audio format.
func (p *OpenAICompat) Synthesize(ctx context.Context, in audio.SpeechRequest) (audio.SpeechResponse, error) {
	body, err := json.Marshal(struct {
		Model          string `json:"model"`
		Input          string `json:"input"`
		Voice          string `json:"voice"`
		ResponseFormat string `json:"response_format,omitempty"`
	}{Model: in.Model, Input: in.Input, Voice: in.Voice, ResponseFormat: in.ResponseFormat})
	if err != nil {
		return audio.SpeechResponse{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/audio/speech", bytes.NewReader(body))
	if err != nil {
		return audio.SpeechResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if p.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.apiKey)
	}

	resp, err := p.hc.Do(req)
	if err != nil {
		return audio.SpeechResponse{}, &Error{Status: http.StatusBadGateway, Retryable: true, Message: err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return audio.SpeechResponse{}, httpError(p.name, resp.StatusCode, b)
	}
	audioBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return audio.SpeechResponse{}, err
	}
	return audio.SpeechResponse{Audio: audioBytes, ContentType: resp.Header.Get("Content-Type")}, nil
}
```

- [ ] **Step 4: Run the full suite**

Run: `gofmt -l . && go build ./... && go vet ./... && go test ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/providers/openai_compat.go internal/providers/openai_compat_transcribe_test.go
git commit -m "feat(audio): OpenAICompat.Transcribe/Synthesize (works against Groq, OpenAI, or a local OpenAI-shaped audio server)"
```

---

### Task 3: Migration 0011 + pricing.Unit + audio cost helpers

**Files:**
- Create: `migrations/0011_audio_pricing_and_dlp.sql`
- Modify: `internal/pricing/pricing.go`
- Test: `internal/pricing/pricing_test.go` (extend)

**Interfaces:**
- Produces: `pricing.Price.Unit string`; `(*Table).AudioCostMicroUSD(provider, model string, seconds float64) int64`; `(*Table).TTSCostMicroUSD(provider, model string, chars int) int64`. Consumed by Task 6.
- Also produces the `model_aliases.dlp_audio_scan` column Task 5's routing read uses.

- [ ] **Step 1: Write the migration**

Create `migrations/0011_audio_pricing_and_dlp.sql`:

```sql
-- Batch audio (STT/TTS): pricing needs non-token units, and audio DLP
-- scanning is a separate per-alias toggle from the chat layer-2 gate.
ALTER TABLE pricing ADD COLUMN unit text NOT NULL DEFAULT 'tokens'
    CHECK (unit IN ('tokens', 'audio_second', 'text_char'));
ALTER TABLE model_aliases ADD COLUMN dlp_audio_scan boolean NOT NULL DEFAULT true;
```

- [ ] **Step 2: Write the failing tests**

Add to `internal/pricing/pricing_test.go`:

```go
func TestAudioCostMicroUSD(t *testing.T) {
	tab := New()
	// $0.006/minute == $0.0001/second == $100 per 1M seconds.
	tab.Set("groq", "whisper-large-v3", Price{InputPer1M: 100, Unit: "audio_second"})

	got := tab.AudioCostMicroUSD("groq", "whisper-large-v3", 30) // 30s clip
	want := int64(3000) // 30/1e6*100 USD = 0.003 USD = 3000 microUSD
	if got != want {
		t.Errorf("AudioCostMicroUSD = %d, want %d", got, want)
	}

	if got := tab.AudioCostMicroUSD("groq", "unknown-model", 30); got != 0 {
		t.Errorf("unknown model = %d, want 0", got)
	}
}

func TestTTSCostMicroUSD(t *testing.T) {
	tab := New()
	// OpenAI's own convention: $15.00 / 1M characters.
	tab.Set("openai", "tts-1", Price{InputPer1M: 15, Unit: "text_char"})

	got := tab.TTSCostMicroUSD("openai", "tts-1", 100) // 100-char input
	want := int64(1500) // 100/1e6*15 USD = 0.0015 USD = 1500 microUSD
	if got != want {
		t.Errorf("TTSCostMicroUSD = %d, want %d", got, want)
	}
}

func TestPriceUnitRoundTrip(t *testing.T) {
	tab := New()
	tab.Set("groq", "m", Price{InputPer1M: 5, OutputPer1M: 0, Unit: "audio_second"})
	// Set/lookup round-trips Unit alongside the existing fields — a caller
	// using the wrong cost method for a row's unit is a caller bug, not
	// something the table cross-checks (documented in the design spec).
	got := tab.AudioCostMicroUSD("groq", "m", 1e6)
	if got != 5_000_000 {
		t.Errorf("got %d, want 5000000", got)
	}
}
```

- [ ] **Step 3: Run tests to verify they fail**

Run: `go test ./internal/pricing/ -run "TestAudioCostMicroUSD|TestTTSCostMicroUSD|TestPriceUnitRoundTrip" -v`
Expected: FAIL — `Price.Unit` field and the two new methods don't exist yet.

- [ ] **Step 4: Implement**

In `internal/pricing/pricing.go`:

1. Add `Unit` to `Price`:

```go
// Price is the USD cost per 1,000,000 units for a model. Unit says what is
// being counted: "tokens" (default), "audio_second" (STT, priced by audio
// duration), or "text_char" (TTS, priced by input character count) — same
// formula for all three, only the quantity differs.
type Price struct {
	InputPer1M  float64
	OutputPer1M float64
	Unit        string
}
```

2. Update `Load` to read the column and `CostMicroUSD`'s doc comment is unchanged; add the two new methods after `CostMicroUSD`:

```go
// AudioCostMicroUSD prices a transcription by audio duration: seconds/1e6
// * the priced row's InputPer1M (interpreted as $ per 1,000,000 seconds).
// Same lookup precedence as CostMicroUSD: exact provider+model, then the
// wildcard provider "", then 0 for an unpriced model.
func (t *Table) AudioCostMicroUSD(provider, model string, seconds float64) int64 {
	t.mu.RLock()
	p, ok := t.prices[key(provider, model)]
	if !ok {
		p, ok = t.prices[key("", model)]
	}
	t.mu.RUnlock()
	if !ok {
		return 0
	}
	return int64(math.Round(seconds / 1e6 * p.InputPer1M * 1e6))
}

// TTSCostMicroUSD prices a synthesis by input character count: chars/1e6 *
// the priced row's InputPer1M (interpreted as $ per 1,000,000 characters).
// Same lookup precedence as CostMicroUSD.
func (t *Table) TTSCostMicroUSD(provider, model string, chars int) int64 {
	t.mu.RLock()
	p, ok := t.prices[key(provider, model)]
	if !ok {
		p, ok = t.prices[key("", model)]
	}
	t.mu.RUnlock()
	if !ok {
		return 0
	}
	return int64(math.Round(float64(chars) / 1e6 * p.InputPer1M * 1e6))
}
```

3. Update `Load`'s query and scan to include `unit`:

```go
rows, err := st.PG.Query(ctx, `SELECT provider, model, input_per_1m, output_per_1m, unit FROM pricing`)
...
if err := rows.Scan(&provider, &model, &p.InputPer1M, &p.OutputPer1M, &p.Unit); err != nil {
```

- [ ] **Step 5: Run the full suite**

Run: `gofmt -l . && go build ./... && go vet ./... && go test ./...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
docker compose -f deploy/docker-compose.yml up --build -d app   # applies 0011 to the dev DB
git add migrations/0011_audio_pricing_and_dlp.sql internal/pricing/pricing.go internal/pricing/pricing_test.go
git commit -m "feat(pricing): audio_second/text_char price units; migration 0011 (+ dlp_audio_scan column for Task 5)"
```

---

### Task 4: Per-key limits — audio_seconds / tts_chars dimensions

**Files:**
- Modify: `internal/policy/policy.go`
- Modify: `internal/limits/limits.go`
- Test: `internal/limits/limits_test.go` (extend — read it first to match its existing style, e.g. miniredis or a real Redis; follow whatever it already uses)

**Interfaces:**
- Produces: `policy.Limits.AudioSeconds map[string]int64`, `policy.Limits.TTSChars map[string]int64`; `(*Limiter).Check` and `(*Limiter).Add` gain two more quantities. Consumed by Task 6.

- [ ] **Step 1: Note on testing (read before writing any test)**

`internal/limits/limits_test.go` currently tests only the pure helpers
(`BucketStamp`, `SumWindows`, `expiredFields`) — there is no existing
harness that constructs a real `*Limiter` against Redis anywhere in this
codebase (`Check`/`Add` are exercised live, not in `go test`, same
precedent as the chat handlers — see Task 6's note). Follow that
precedent here too: no new Redis test harness for this task. `Check`'s
window-comparison logic for the two new dimensions is a straight copy of
the existing tokens/cost branches (same shape, different map/field names),
low-risk enough to cover via code review + Task 8's live verification
(which exercises `audio_seconds` against the real dev Redis end to end).

- [ ] **Step 2: Extend policy.Limits**

In `internal/policy/policy.go`:

```go
// Limits are per-window usage caps. Keys are window names ("5h"/"24h"/"7d");
// a missing or non-positive value means no cap for that window/unit.
type Limits struct {
	Tokens       map[string]int64   `json:"tokens,omitempty"`
	CostUSD      map[string]float64 `json:"cost_usd,omitempty"`
	AudioSeconds map[string]int64   `json:"audio_seconds,omitempty"`
	TTSChars     map[string]int64   `json:"tts_chars,omitempty"`
}
```

- [ ] **Step 3: Implement** (no failing-test step first for this task — see Step 1's note)

In `internal/limits/limits.go`:

1. Add key builders next to `tokKey`/`costKey`:

```go
func audioSecKey(key string) string { return "air:u:" + key + ":asec" }
func ttsCharKey(key string) string  { return "air:u:" + key + ":ttsc" }
```

2. Extend `Check`:

```go
func (l *Limiter) Check(ctx context.Context, key string, lim policy.Limits) (Decision, error) {
	if len(lim.Tokens) == 0 && len(lim.CostUSD) == 0 && len(lim.AudioSeconds) == 0 && len(lim.TTSChars) == 0 {
		return Decision{Allowed: true}, nil
	}

	now := l.now()
	tokFields, err := l.rdb.HGetAll(ctx, tokKey(key)).Result()
	if err != nil {
		return Decision{Allowed: true}, err
	}
	costFields, err := l.rdb.HGetAll(ctx, costKey(key)).Result()
	if err != nil {
		return Decision{Allowed: true}, err
	}
	audioFields, err := l.rdb.HGetAll(ctx, audioSecKey(key)).Result()
	if err != nil {
		return Decision{Allowed: true}, err
	}
	ttsFields, err := l.rdb.HGetAll(ctx, ttsCharKey(key)).Result()
	if err != nil {
		return Decision{Allowed: true}, err
	}

	l.prune(ctx, key, now, tokFields, costFields, audioFields, ttsFields)

	tokSums := SumWindows(now, tokFields)
	costSums := SumWindows(now, costFields)
	audioSums := SumWindows(now, audioFields)
	ttsSums := SumWindows(now, ttsFields)

	for _, win := range Windows {
		if max, ok := lim.Tokens[win.Name]; ok && max > 0 && tokSums[win.Name] >= max {
			return Decision{Allowed: false, Window: win.Name, Unit: "tokens", Limit: max, Used: tokSums[win.Name]}, nil
		}
		if usd, ok := lim.CostUSD[win.Name]; ok && usd > 0 {
			maxMicro := int64(usd * 1e6)
			if costSums[win.Name] >= maxMicro {
				return Decision{Allowed: false, Window: win.Name, Unit: "cost_usd", Limit: maxMicro, Used: costSums[win.Name]}, nil
			}
		}
		if max, ok := lim.AudioSeconds[win.Name]; ok && max > 0 && audioSums[win.Name] >= max {
			return Decision{Allowed: false, Window: win.Name, Unit: "audio_seconds", Limit: max, Used: audioSums[win.Name]}, nil
		}
		if max, ok := lim.TTSChars[win.Name]; ok && max > 0 && ttsSums[win.Name] >= max {
			return Decision{Allowed: false, Window: win.Name, Unit: "tts_chars", Limit: max, Used: ttsSums[win.Name]}, nil
		}
	}
	return Decision{Allowed: true}, nil
}
```

3. Extend `Add`'s signature (update its doc comment too):

```go
// Add increments the current bucket with the given usage and refreshes the
// hash TTLs so idle keys eventually expire. Any zero-valued quantity is
// skipped (no Redis write for a dimension a request didn't use).
func (l *Limiter) Add(ctx context.Context, key string, tokens, costMicroUSD, audioSeconds, ttsChars int64) error {
	if tokens == 0 && costMicroUSD == 0 && audioSeconds == 0 && ttsChars == 0 {
		return nil
	}
	field := strconv.FormatInt(BucketStamp(l.now()), 10)
	ttl := maxWindow() + 2*BucketSize

	pipe := l.rdb.Pipeline()
	if tokens != 0 {
		pipe.HIncrBy(ctx, tokKey(key), field, tokens)
		pipe.Expire(ctx, tokKey(key), ttl)
	}
	if costMicroUSD != 0 {
		pipe.HIncrBy(ctx, costKey(key), field, costMicroUSD)
		pipe.Expire(ctx, costKey(key), ttl)
	}
	if audioSeconds != 0 {
		pipe.HIncrBy(ctx, audioSecKey(key), field, audioSeconds)
		pipe.Expire(ctx, audioSecKey(key), ttl)
	}
	if ttsChars != 0 {
		pipe.HIncrBy(ctx, ttsCharKey(key), field, ttsChars)
		pipe.Expire(ctx, ttsCharKey(key), ttl)
	}
	_, err := pipe.Exec(ctx)
	return err
}
```

4. Extend `prune`'s signature and body (update its doc comment):

```go
func (l *Limiter) prune(ctx context.Context, key string, now time.Time, tokFields, costFields, audioFields, ttsFields map[string]string) {
	cutoff := now.Add(-maxWindow() - BucketSize).Unix()
	tokExpired := expiredFields(cutoff, tokFields)
	costExpired := expiredFields(cutoff, costFields)
	audioExpired := expiredFields(cutoff, audioFields)
	ttsExpired := expiredFields(cutoff, ttsFields)
	if len(tokExpired) == 0 && len(costExpired) == 0 && len(audioExpired) == 0 && len(ttsExpired) == 0 {
		return
	}
	pipe := l.rdb.Pipeline()
	if len(tokExpired) > 0 {
		pipe.HDel(ctx, tokKey(key), tokExpired...)
	}
	if len(costExpired) > 0 {
		pipe.HDel(ctx, costKey(key), costExpired...)
	}
	if len(audioExpired) > 0 {
		pipe.HDel(ctx, audioSecKey(key), audioExpired...)
	}
	if len(ttsExpired) > 0 {
		pipe.HDel(ctx, ttsCharKey(key), ttsExpired...)
	}
	_, _ = pipe.Exec(ctx)
}
```

- [ ] **Step 4: Fix the existing call sites**

`Add`'s signature changed from 3 args to 5. Find every caller:

Run: `grep -rn "\.Add(ctx" internal/httpapi/*.go internal/limits/*.go | grep -v _test`

Update each existing call (e.g. in `internal/httpapi/exec.go`'s `finalizeUsage`) from
`s.limiter.Add(ctx, keyID, int64(prompt+completion), costMicro)` to
`s.limiter.Add(ctx, keyID, int64(prompt+completion), costMicro, 0, 0)` (chat requests never touch the two new dimensions).

- [ ] **Step 5: Run the full suite**

Run: `gofmt -l . && go build ./... && go vet ./... && go test ./...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/policy/policy.go internal/limits/limits.go internal/httpapi/exec.go
git commit -m "feat(limits): audio_seconds/tts_chars as explicit limit dimensions, same pattern as tokens/cost"
```

---

### Task 5: dlpScanText (layer-1 DLP for a single string) + the dlp_audio_scan routing gate

**Files:**
- Modify: `internal/httpapi/dlp.go`
- Modify: `internal/routing/routing.go` (the per-alias gate this task's DLP function needs — same file/pattern as `DLPModelScan`/`ExposeBackendHeaders`)
- Test: `internal/httpapi/dlp_guardrails_test.go` (extend, reusing `newDLPEnforceTestServer`'s exact construction pattern already in that file)
- Test: `internal/routing/routing_flag_test.go` (extend, same gated pattern as `TestDLPModelScanFlag`/`TestDisplayLabelFlag`)

**Interfaces:**
- Consumes: `s.dlpCfg()`, `dlpToggle`, `recordDLP`, `actionPast`, `excerpt`, `sortedKeys`, `dlp.Labels` (all existing, unchanged).
- Produces: `(s *Server) dlpScanText(ctx context.Context, ak authedKey, ingress, text string) (blocked bool, message string, findings []dlp.Finding, redactedText string)`; `routing.Plan.DLPAudioScan bool`. Both consumed by Task 6 (the audio handlers must skip calling `dlpScanText` when `plan.DLPAudioScan` is false).

- [ ] **Step 1: Add the routing gate (migration 0011 already added the column in Task 3)**

In `internal/routing/routing.go`, add to `Plan` (next to `DLPModelScan`):

```go
DLPAudioScan bool // run layer-1 DLP scanning on audio text for this alias
```

In the passthrough branch of `Resolve` (same reasoning as `DLPModelScan: true`
there — an explicit `provider/model` request scans by default too):

```go
return &Plan{Alias: model, Strategy: "round_robin", DLPModelScan: true, ExposeBackendHeaders: true, DLPAudioScan: true, Tiers: [][]Target{{t}}}, nil
```

In the alias branch, extend the query and scan:

```go
var strategy string
var dlpModelScan, exposeBackendHeaders, dlpAudioScan bool
err := r.st.PG.QueryRow(ctx, `SELECT strategy, dlp_model_scan, expose_backend_headers, dlp_audio_scan FROM model_aliases WHERE alias = $1`, model).Scan(&strategy, &dlpModelScan, &exposeBackendHeaders, &dlpAudioScan)
```

and the final `Plan{...}` construction:

```go
return &Plan{Alias: model, Strategy: strategy, DLPModelScan: dlpModelScan, ExposeBackendHeaders: exposeBackendHeaders, DLPAudioScan: dlpAudioScan, Tiers: tiers}, nil
```

(Check the exact current field list on that `QueryRow`/`Plan{...}` line
first — Task 3 already added `expose_backend_headers` there in an earlier
release; this step ADDS `dlp_audio_scan` alongside it, it does not replace
anything.)

- [ ] **Step 2: Gated routing test**

Add to `internal/routing/routing_flag_test.go`, mirroring `TestDisplayLabelFlag`'s
exact structure (tx-scoped fixture, skip if the column is missing):

```go
func TestDLPAudioScanFlag(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `SELECT dlp_audio_scan FROM model_aliases LIMIT 0`); err != nil {
		t.Skipf("dlp_audio_scan column not present (migration 0011 not applied?): %v", err)
	}

	mustExec := func(sql string, args ...any) {
		t.Helper()
		if _, err := tx.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %s: %v", sql, err)
		}
	}
	mustExec(`INSERT INTO providers (name, kind, base_url, enabled) VALUES ($1, $2, $3, $4)`,
		"audioflag-test-provider", "openai", "http://example.invalid", true)
	mustExec(`INSERT INTO model_aliases (alias, protocol, strategy, dlp_audio_scan) VALUES ($1, $2, $3, $4)`,
		"audioflag-test-alias", "openai", "round_robin", false)
	mustExec(`INSERT INTO alias_targets (alias, priority, provider_name, upstream_model, upstream_protocol) VALUES ($1, $2, $3, $4, $5)`,
		"audioflag-test-alias", 0, "audioflag-test-provider", "upstream-model", "openai")

	var strategy string
	var dlpAudioScan bool
	if err := tx.QueryRow(ctx,
		`SELECT strategy, dlp_audio_scan FROM model_aliases WHERE alias = $1`, "audioflag-test-alias",
	).Scan(&strategy, &dlpAudioScan); err != nil {
		t.Fatalf("query alias: %v", err)
	}
	if dlpAudioScan {
		t.Errorf("dlp_audio_scan = true, want false for audioflag-test-alias")
	}
}
```

- [ ] **Step 3: Write the failing tests for dlpScanText**

First read `newDLPEnforceTestServer` in `internal/httpapi/dlp_guardrails_test.go`
(already present — builds a `*Server` with a never-connecting dummy pgx pool
and `s.dlpPtr.Store(&cfg)`). Add a small sibling helper right after it, since
`dlpScanText` needs no sidecar/modelPool at all:

```go
// newDLPScanTextTestServer builds a *Server for dlpScanText tests: same
// dummy-pool trick as newDLPEnforceTestServer, no sidecar/modelPool since
// dlpScanText never runs the layer-2 model scan.
func newDLPScanTextTestServer(t *testing.T, cfg dlpConfig) *Server {
	t.Helper()
	pgPool, err := pgxpool.New(context.Background(), "postgres://user:pass@127.0.0.1:1/db?connect_timeout=1")
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pgPool.Close)
	s := &Server{st: &store.Store{PG: pgPool}, httpc: http.DefaultClient}
	s.dlpPtr.Store(&cfg)
	return s
}

func TestDlpScanTextFlag(t *testing.T) {
	s := newDLPScanTextTestServer(t, dlpConfig{Enabled: true, Action: "flag"})
	blocked, msg, findings, redacted := s.dlpScanText(context.Background(), authedKey{}, "openai", "my key is sk-test-1234567890abcdef1234567890abcdef")
	if blocked {
		t.Errorf("action=flag must not block, got blocked=%v msg=%q", blocked, msg)
	}
	if len(findings) == 0 {
		t.Error("want at least one finding for a planted secret")
	}
	if redacted != "my key is sk-test-1234567890abcdef1234567890abcdef" {
		t.Errorf("action=flag must not alter the text, got %q", redacted)
	}
}

func TestDlpScanTextRedact(t *testing.T) {
	s := newDLPScanTextTestServer(t, dlpConfig{Enabled: true, Action: "redact"})
	_, _, findings, redacted := s.dlpScanText(context.Background(), authedKey{}, "openai", "my key is sk-test-1234567890abcdef1234567890abcdef")
	if len(findings) == 0 {
		t.Fatal("want at least one finding")
	}
	if redacted == "my key is sk-test-1234567890abcdef1234567890abcdef" {
		t.Error("action=redact must alter the text")
	}
}

func TestDlpScanTextBlock(t *testing.T) {
	s := newDLPScanTextTestServer(t, dlpConfig{Enabled: true, Action: "block"})
	blocked, msg, _, _ := s.dlpScanText(context.Background(), authedKey{}, "openai", "my key is sk-test-1234567890abcdef1234567890abcdef")
	if !blocked {
		t.Error("action=block must block a planted secret")
	}
	if msg == "" {
		t.Error("want a non-empty block message")
	}
}

func TestDlpScanTextClean(t *testing.T) {
	s := newDLPScanTextTestServer(t, dlpConfig{Enabled: true, Action: "block"})
	blocked, _, findings, redacted := s.dlpScanText(context.Background(), authedKey{}, "openai", "just an ordinary sentence")
	if blocked {
		t.Error("clean text must not block")
	}
	if len(findings) != 0 {
		t.Errorf("want no findings, got %+v", findings)
	}
	if redacted != "just an ordinary sentence" {
		t.Errorf("clean text must round-trip unchanged, got %q", redacted)
	}
}
```

- [ ] **Step 4: Run tests to verify they fail**

Run: `go test ./internal/httpapi/ -run TestDlpScanText -v`
Expected: FAIL — `dlpScanText` undefined.

- [ ] **Step 5: Implement dlpScanText**

In `internal/httpapi/dlp.go`, add after `dlpEnforce` (or its helpers — place it near `dlpEnforce` for discoverability):

```go
// dlpScanText runs layer-1 deterministic DLP (regex + entropy) over a single
// string — the shape audio requests need (one transcript, or one TTS input,
// not a chat message array). Unlike dlpEnforce it never runs the layer-2
// BERT model scan (audio DLP is layer-1 only, per the design spec) and
// takes no modelScan/budget parameters. cfg.Action drives the same
// flag/redact/block semantics as dlpEnforce.
func (s *Server) dlpScanText(ctx context.Context, ak authedKey, ingress, text string) (blocked bool, message string, findings []dlp.Finding, redactedText string) {
	cfg := s.dlpCfg()
	redactedText = text
	if !cfg.Enabled || cfg.Action == "off" || text == "" {
		return false, "", nil, redactedText
	}

	findings = dlp.ScanWith(text, dlp.PatternSet{
		Enabled: cfg.Patterns,
		Custom:  cfg.compiledCustom,
		Entropy: dlpToggle(cfg.Patterns, "high_entropy", true),
	})
	if len(findings) == 0 {
		return false, "", nil, redactedText
	}

	labels := sortedKeys(labelSetOf(findings))
	sample := excerpt(dlp.Redact(text, findings))
	s.recordDLP(ctx, ak, ingress, "audio", actionPast(cfg.Action), labels, len(findings), sample)

	if cfg.Action == "block" {
		return true, "request blocked: sensitive content detected (" + strings.Join(labels, ", ") + ")", findings, redactedText
	}
	if cfg.Action == "redact" {
		redactedText = dlp.Redact(text, findings)
	}
	return false, "", findings, redactedText
}

// labelSetOf collects the distinct labels across a findings slice, matching
// the label-collection loop dlpEnforce runs inline per message.
func labelSetOf(findings []dlp.Finding) map[string]bool {
	set := map[string]bool{}
	for _, l := range dlp.Labels(findings) {
		set[l] = true
	}
	return set
}
```

Check `dlp.Labels`'s exact signature first (`grep -n "func Labels" internal/dlp/dlp.go`) — it takes `[]dlp.Finding` and returns `[]string`, same as `dlpEnforce` already calls it; if the real signature differs, adjust `labelSetOf` accordingly rather than guessing further.

- [ ] **Step 6: Run the full suite**

Run: `gofmt -l . && go build ./... && go vet ./... && go test ./...`
Expected: PASS. Also run the gated routing test:
`TEST_DATABASE_URL="postgres://airllm:airllm@127.0.0.1:55432/airllm?sslmode=disable" go test ./internal/routing/ -run TestDLPAudioScanFlag -v`
(only if the dev Postgres from Task 3's migration is up; otherwise it self-skips).

- [ ] **Step 7: Commit**

```bash
git add internal/httpapi/dlp.go internal/httpapi/dlp_guardrails_test.go internal/routing/routing.go internal/routing/routing_flag_test.go
git commit -m "feat(dlp): dlpScanText — layer-1-only DLP for a single string (audio transcripts/input)"
```

---

### Task 6: Ingress — POST /v1/audio/transcriptions + POST /v1/audio/speech

**Files:**
- Create: `internal/httpapi/api_audio.go`
- Modify: `internal/httpapi/server.go` (route registration, ~line 194)

**Interfaces:**
- Consumes: `audio.TranscriptionRequest/Response`, `audio.SpeechRequest/Response` (Task 1); `(*OpenAICompat).Transcribe/Synthesize` reachable via the `providers.Transcriber`/`Synthesizer` interfaces (Task 2); `pricing.Table.AudioCostMicroUSD/TTSCostMicroUSD` (Task 3); `policy.Limits.AudioSeconds/TTSChars` + `Limiter.Check/Add` (Task 4); `(s *Server) dlpScanText`, `plan.DLPAudioScan` (Task 5); `routing.Resolve`, `s.reg()`, `authedKey`, `ledger.Entry` fields, `writeProtocolError`, `writeJSON`, `decodeJSON` (all existing).
- Produces: `s.handleAudioTranscriptions`, `s.handleAudioSpeech` — routed, nothing else depends on these names.

**No dedicated test file for this task.** Checked before writing this
plan: `handleChatCompletions` and `handleMessages` — the two existing
top-level data-plane handlers this task's shape directly mirrors — have
**no** Go tests of their own anywhere in this codebase either (no
`dataplane_test.go`/`messages_test.go`); their correctness is verified live
(curl/Playwright), because a real test needs router+registry+limiter+DB+
Redis all wired together and no such harness exists for this package.
Task 1/2/3/5 already cover the pieces this handler composes in isolation;
Task 8 covers the composed handler live, the same way chat's handlers are
covered. Building a one-off full-stack harness just for this task would be
new, disproportionate infrastructure relative to that precedent — skip it.

- [ ] **Step 1: Implement**

Create `internal/httpapi/api_audio.go`:

```go
package httpapi

import (
	"io"
	"net/http"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/audio"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/ledger"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/providers"
)

// handleAudioTranscriptions implements POST /v1/audio/transcriptions:
// multipart upload, alias-routed like chat, batch (no streaming). Response
// format is always {"text": "..."} in v1 regardless of what the client
// requests — see the design spec's out-of-scope list.
func (s *Server) handleAudioTranscriptions(w http.ResponseWriter, r *http.Request) {
	ak, _ := keyFromContext(r.Context())
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeProtocolError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid multipart body: "+err.Error())
		return
	}
	model := r.FormValue("model")
	if model == "" {
		writeProtocolError(w, r, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}
	if !ak.Policy.Allows(model) {
		writeProtocolError(w, r, http.StatusForbidden, "permission_error", "model not permitted for this key: "+model)
		return
	}
	file, hdr, err := r.FormFile("file")
	if err != nil {
		writeProtocolError(w, r, http.StatusBadRequest, "invalid_request_error", "file is required")
		return
	}
	defer file.Close()
	audioBytes, err := io.ReadAll(file)
	if err != nil {
		writeProtocolError(w, r, http.StatusBadRequest, "invalid_request_error", "failed to read file")
		return
	}

	plan, err := s.router.Resolve(r.Context(), model, ak.Policy.AllowPassthrough)
	if err != nil {
		writeProtocolError(w, r, http.StatusNotFound, "invalid_request_error", err.Error())
		return
	}
	if msg, denied := s.limitDenied(r.Context(), ak); denied {
		writeProtocolError(w, r, http.StatusTooManyRequests, "rate_limit_error", msg)
		return
	}

	reg := s.reg()
	var resp audio.TranscriptionResponse
	var target string
	var upstreamModel string
	var callErr error
	for _, t := range plan.Ordered(s.router.NextRR(plan.Alias), s.freeFunc(reg)) {
		e, ok := reg.Get(t.Provider)
		if !ok {
			continue
		}
		tr, ok := e.Provider.(providers.Transcriber)
		if !ok {
			callErr = &providers.Error{Status: http.StatusBadRequest, Retryable: false, Message: "provider " + t.Provider + " does not support transcription"}
			continue
		}
		if !e.Acquire() {
			continue
		}
		resp, callErr = tr.Transcribe(r.Context(), audio.TranscriptionRequest{
			Model: t.UpstreamModel, Audio: audioBytes, Filename: hdr.Filename,
			Language: r.FormValue("language"), Prompt: r.FormValue("prompt"),
		})
		e.Release()
		target, upstreamModel = t.Provider, t.UpstreamModel
		if callErr == nil {
			break
		}
		if !providers.IsRetryable(callErr) {
			break
		}
	}
	if callErr != nil {
		code, typ := classifyUpstreamErr(callErr)
		if pe, ok := callErr.(*providers.Error); ok && !pe.Retryable {
			code = pe.Status
		}
		writeProtocolError(w, r, code, typ, callErr.Error())
		return
	}

	redactedText := resp.Text
	if plan.DLPAudioScan {
		var blocked bool
		var msg string
		blocked, msg, _, redactedText = s.dlpScanText(r.Context(), ak, "openai", resp.Text)
		if blocked {
			writeProtocolError(w, r, http.StatusBadRequest, "invalid_request_error", msg)
			return
		}
	}

	costMicro := s.pricing.AudioCostMicroUSD(target, upstreamModel, resp.DurationSeconds)
	s.ledger.Record(r.Context(), ledger.Entry{
		KeyID: ak.KeyID, UserID: ak.UserID, Alias: model, ProviderName: target, UpstreamModel: upstreamModel,
		IngressProtocol: "openai", UpstreamProtocol: "openai", Status: http.StatusOK, CostUSD: float64(costMicro) / 1e6,
	})
	if err := s.limiter.Add(r.Context(), ak.KeyID, 0, 0, int64(resp.DurationSeconds), 0); err != nil {
		slog.Error("limiter add failed", "err", err)
	}
	s.enqueueCapture(ak, "openai", model, target, upstreamModel, http.StatusOK, 0, 0, float64(costMicro)/1e6, dlpResult{}, nil, redactedText)

	writeJSON(w, http.StatusOK, map[string]string{"text": redactedText})
}

// handleAudioSpeech implements POST /v1/audio/speech: JSON in, raw audio
// bytes out, batch (no streaming).
func (s *Server) handleAudioSpeech(w http.ResponseWriter, r *http.Request) {
	ak, _ := keyFromContext(r.Context())
	var body struct {
		Model          string `json:"model"`
		Input          string `json:"input"`
		Voice          string `json:"voice"`
		ResponseFormat string `json:"response_format"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeProtocolError(w, r, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if body.Model == "" || body.Input == "" {
		writeProtocolError(w, r, http.StatusBadRequest, "invalid_request_error", "model and input are required")
		return
	}
	if !ak.Policy.Allows(body.Model) {
		writeProtocolError(w, r, http.StatusForbidden, "permission_error", "model not permitted for this key: "+body.Model)
		return
	}

	plan, err := s.router.Resolve(r.Context(), body.Model, ak.Policy.AllowPassthrough)
	if err != nil {
		writeProtocolError(w, r, http.StatusNotFound, "invalid_request_error", err.Error())
		return
	}
	if msg, denied := s.limitDenied(r.Context(), ak); denied {
		writeProtocolError(w, r, http.StatusTooManyRequests, "rate_limit_error", msg)
		return
	}

	redactedInput := body.Input
	if plan.DLPAudioScan {
		var blocked bool
		var msg string
		blocked, msg, _, redactedInput = s.dlpScanText(r.Context(), ak, "openai", body.Input)
		if blocked {
			writeProtocolError(w, r, http.StatusBadRequest, "invalid_request_error", msg)
			return
		}
	}

	reg := s.reg()
	var resp audio.SpeechResponse
	var target string
	var upstreamModel string
	var callErr error
	for _, t := range plan.Ordered(s.router.NextRR(plan.Alias), s.freeFunc(reg)) {
		e, ok := reg.Get(t.Provider)
		if !ok {
			continue
		}
		sy, ok := e.Provider.(providers.Synthesizer)
		if !ok {
			callErr = &providers.Error{Status: http.StatusBadRequest, Retryable: false, Message: "provider " + t.Provider + " does not support speech synthesis"}
			continue
		}
		if !e.Acquire() {
			continue
		}
		resp, callErr = sy.Synthesize(r.Context(), audio.SpeechRequest{
			Model: t.UpstreamModel, Input: redactedInput, Voice: body.Voice, ResponseFormat: body.ResponseFormat,
		})
		e.Release()
		target, upstreamModel = t.Provider, t.UpstreamModel
		if callErr == nil {
			break
		}
		if !providers.IsRetryable(callErr) {
			break
		}
	}
	if callErr != nil {
		code, typ := classifyUpstreamErr(callErr)
		if pe, ok := callErr.(*providers.Error); ok && !pe.Retryable {
			code = pe.Status
		}
		writeProtocolError(w, r, code, typ, callErr.Error())
		return
	}

	costMicro := s.pricing.TTSCostMicroUSD(target, upstreamModel, len(redactedInput))
	s.ledger.Record(r.Context(), ledger.Entry{
		KeyID: ak.KeyID, UserID: ak.UserID, Alias: body.Model, ProviderName: target, UpstreamModel: upstreamModel,
		IngressProtocol: "openai", UpstreamProtocol: "openai", Status: http.StatusOK, CostUSD: float64(costMicro) / 1e6,
	})
	if err := s.limiter.Add(r.Context(), ak.KeyID, 0, 0, 0, int64(len(redactedInput))); err != nil {
		slog.Error("limiter add failed", "err", err)
	}
	s.enqueueCapture(ak, "openai", body.Model, target, upstreamModel, http.StatusOK, 0, 0, float64(costMicro)/1e6, dlpResult{},
		[]llm.Message{{Role: "user", Content: redactedInput}}, "")

	if resp.ContentType != "" {
		w.Header().Set("Content-Type", resp.ContentType)
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(resp.Audio)
}
```

Before finalizing, check the EXACT `classifyUpstreamErr` signature and the
`ledger.Entry` field names against `internal/httpapi/exec.go` /
`internal/ledger/ledger.go` — this plan was written against those files as
read earlier in this session; if a field name differs (unlikely, but
verify), fix the struct literal to match, don't guess. Also add
`"log/slog"` and `"github.com/ipsupport-llc/ipsupport-airllm/internal/llm"`
to the imports above if not already present (the TTS capture call uses
`llm.Message`, and both handlers log via `slog.Error` on a limiter
failure, matching `finalizeUsage`'s existing pattern in `exec.go`).

- [ ] **Step 2: Register the routes**

In `internal/httpapi/server.go`, after the existing `/v1/messages` line (~194):

```go
s.mux.HandleFunc("POST /v1/audio/transcriptions", s.requireAPIKey(s.handleAudioTranscriptions))
s.mux.HandleFunc("POST /v1/audio/speech", s.requireAPIKey(s.handleAudioSpeech))
```

- [ ] **Step 3: Run the full suite**

Run: `gofmt -l . && go build ./... && go vet ./... && go test ./...`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/httpapi/api_audio.go internal/httpapi/server.go
git commit -m "feat(audio): POST /v1/audio/transcriptions and /v1/audio/speech ingress"
```

---

### Task 7: Console — pricing unit selector

**Files:**
- Modify: `internal/httpapi/api_admin.go` (`handleAdminPricing` ~480, `handleAdminPutPricing` ~506)
- Modify: `web/static/app.js` (`adminPricing` ~871, `editPrice` ~900)

**Interfaces:**
- Consumes: `pricing.Price.Unit` (Task 3).

- [ ] **Step 1: Backend — read/write the unit column**

In `internal/httpapi/api_admin.go`'s `handleAdminPricing`, add `Unit` to the
local `price` struct and to the query/scan:

```go
type price struct {
	Provider    string  `json:"provider"`
	Model       string  `json:"model"`
	InputPer1M  float64 `json:"input_per_1m"`
	OutputPer1M float64 `json:"output_per_1m"`
	Unit        string  `json:"unit"`
}
```

```go
rows, err := s.st.PG.Query(r.Context(),
	`SELECT provider, model, input_per_1m, output_per_1m, unit FROM pricing ORDER BY provider, model`)
...
if err := rows.Scan(&p.Provider, &p.Model, &p.InputPer1M, &p.OutputPer1M, &p.Unit); err != nil {
```

In `handleAdminPutPricing`, add `Unit` to the request body struct (default
`"tokens"` if empty) and to the upsert:

```go
var body struct {
	Provider    string  `json:"provider"`
	InputPer1M  float64 `json:"input_per_1m"`
	OutputPer1M float64 `json:"output_per_1m"`
	Unit        string  `json:"unit"`
}
if err := decodeJSON(r, &body); err != nil {
	writeControlError(w, http.StatusBadRequest, "invalid body")
	return
}
if body.Unit == "" {
	body.Unit = "tokens"
}
_, err := s.st.PG.Exec(r.Context(), `
	INSERT INTO pricing (provider, model, input_per_1m, output_per_1m, unit)
	VALUES ($1, $2, $3, $4, $5)
	ON CONFLICT (provider, model) DO UPDATE SET
		input_per_1m = EXCLUDED.input_per_1m, output_per_1m = EXCLUDED.output_per_1m,
		unit = EXCLUDED.unit, updated_at = now()`,
	body.Provider, model, body.InputPer1M, body.OutputPer1M, body.Unit)
...
s.pricing.Set(body.Provider, model, pricing.Price{InputPer1M: body.InputPer1M, OutputPer1M: body.OutputPer1M, Unit: body.Unit})
```

- [ ] **Step 2: Console — unit column + selector**

In `web/static/app.js`'s `adminPricing`, add a Unit column:

```js
panelTable("Pricing", ["Provider", "Model", "Unit", "Input", "Output", ""],
  ps.map((p) => `<tr><td class="mono">${esc(p.provider) || "(any)"}</td><td class="mono">${esc(p.model)}</td>
    <td>${esc(p.unit || "tokens")}</td><td>${p.input_per_1m}</td><td>${p.output_per_1m}</td>
    <td style="text-align:right"><button class="btn ghost sm" data-edit='${esc(JSON.stringify(p))}'>Edit</button></td></tr>`));
```

(Change the title from `"Pricing (USD / 1M tokens)"` to `"Pricing"` since
the unit is no longer always tokens — put the "$/1M of the row's unit"
framing in the new column header instead of the panel title.)

In `editPrice`, add a Unit select and send it through:

```js
modalForm(p.model ? `Edit price ${p.model}` : "New price", [
  { name: "model", label: "Upstream model", value: p.model || "", disabled: !!p.model },
  { name: "provider", label: "Provider", type: "select", options: providerOptions, value: p.provider || "", disabled: !!p.model },
  { name: "unit", label: "Unit ($ / 1M of this)", type: "select",
    options: ["tokens", "audio_second", "text_char"], value: p.unit || "tokens" },
  { name: "input_per_1m", label: "Input $ / 1M", value: p.input_per_1m ?? 0 },
  { name: "output_per_1m", label: "Output $ / 1M", value: p.output_per_1m ?? 0 },
], async (v) => {
  const x = await api("PUT", `/api/admin/pricing/${encodeURIComponent(v.model)}`,
    { provider: v.provider, unit: v.unit, input_per_1m: Number(v.input_per_1m), output_per_1m: Number(v.output_per_1m) });
  if (x.ok) { toast("Pricing saved"); adminPricing(c); return true; }
  toast((x.data && x.data.error) || "Failed", "err"); return false;
});
```

Check `modalForm`'s `type: "select"` option handling before assuming the
plain-string-array form above renders correctly — the alias editor's Kind
dropdown (`editProvider` in the same file) already uses `options: [...string array...]`,
so this should match that existing convention; if it uses object options
elsewhere (`{value, label}`) instead, match whichever this file already
does for a plain enum-of-strings select.

- [ ] **Step 3: Run + verify**

```bash
node --check web/static/app.js
gofmt -l . && go build ./... && go vet ./... && go test ./...
```
Expected: all clean/PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/httpapi/api_admin.go web/static/app.js
git commit -m "feat(ui): pricing unit selector (tokens/audio_second/text_char) on the pricing tab"
```

---

### Task 8: Live verification (controller)

- [ ] Rebuild the dev app (`docker compose -f deploy/docker-compose.yml up --build -d app`) — applies migration 0011.
- [ ] Extend the scratchpad stub server (or stand up a throwaway `speaches`-shaped container) to serve `POST /audio/transcriptions` (multipart in, `{"text":..., "duration":...}` verbose_json out) and `POST /audio/speech` (JSON in, raw bytes + Content-Type out); register it as a provider; create an alias targeting it.
- [ ] `curl -F model=... -F file=@clip.wav http://<dev>/v1/audio/transcriptions` with a real API key → `{"text": "..."}`, 200.
- [ ] `curl -d '{"model":...,"input":"hello","voice":"..."}' http://<dev>/v1/audio/speech` → raw audio bytes, correct `Content-Type`, 200.
- [ ] Set a price row with `unit=audio_second` for the stub's model; verify `usage_ledger.cost_usd` for the transcription request matches the expected math (duration × price).
- [ ] Plant a fake secret in the stub's transcript response; verify a DLP incident is recorded and (with `action=block`) the request is rejected.
- [ ] Set `role.limits.audio_seconds = {"24h": <tiny number>}` on the test key's role; verify enough transcription calls eventually 429.
- [ ] Point an alias's target at a Mock/chat-only provider and call `/v1/audio/transcriptions` against it → 400, not a panic or a 5xx.
- [ ] Playwright: pricing tab shows the Unit column and the edit modal's Unit selector round-trips.
- [ ] Full existing e2e regression (chat + tool-calls) still green — nothing in this feature touches the chat path except `Limiter.Add`'s signature and `dlp.go`'s new function, both additive.
