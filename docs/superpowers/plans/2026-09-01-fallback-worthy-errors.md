# Fallback-Worthy Errors Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let `runChat`/`runStream` try the next fallback tier when the current tier's error is a known, non-retryable-but-target-specific failure (context length exceeded, model not found), instead of aborting the whole request immediately.

**Architecture:** `providers.Error` gains a `Code string` field, set by best-effort parsing of two known vendor error-body shapes inside `httpError`. A new `providers.IsFallbackWorthy` extends the existing `IsRetryable` check with a lookup against two recognized codes. `runChat`/`runStream`'s single retry-gate call site each switch from `IsRetryable` to `IsFallbackWorthy`; `classifyUpstreamErr` gains a branch so a fully-exhausted request with a recognized code returns a `400` instead of the generic `502`.

**Tech Stack:** Go stdlib `encoding/json`, existing `providers`/`httpapi` packages — no new dependencies.

**Spec:** docs/superpowers/specs/2026-09-01-fallback-worthy-errors-design.md

## Global Constraints

- `Retryable` keeps its exact current meaning and every existing call site that only checks it is unaffected — this plan is purely additive.
- Only two error codes are recognized: `context_length_exceeded`, `model_not_found`. No other vendor shapes, no blanket "all 4xx fall back" behavior.
- Audio handlers (`internal/httpapi/api_audio.go`) are explicitly untouched — this plan does not extend to STT/TTS.
- A body that doesn't parse as either known shape leaves `Code` empty and behaves exactly as today (fail-safe, no behavior change).
- `gofmt -l .` clean before every commit; `go build ./... && go vet ./... && go test ./...` green.

---

### Task 1: Error taxonomy — `Code` field, known codes, `IsFallbackWorthy`

**Files:**
- Modify: `internal/providers/errors.go`
- Test: `internal/providers/errors_test.go` (new file)

**Interfaces:**
- Produces (consumed by Tasks 2, 3, 4):
  - `providers.Error.Code string` (new field on the existing struct)
  - `providers.ErrCodeContextLengthExceeded = "context_length_exceeded"` (const)
  - `providers.ErrCodeModelNotFound = "model_not_found"` (const)
  - `providers.IsFallbackWorthy(err error) bool`

- [ ] **Step 1: Write the failing tests**

Create `internal/providers/errors_test.go`:

```go
package providers

import (
	"errors"
	"testing"
)

func TestIsFallbackWorthyRetryable(t *testing.T) {
	err := &Error{Status: 503, Retryable: true}
	if !IsFallbackWorthy(err) {
		t.Error("a retryable error must be fallback-worthy")
	}
}

func TestIsFallbackWorthyKnownCode(t *testing.T) {
	for _, code := range []string{ErrCodeContextLengthExceeded, ErrCodeModelNotFound} {
		err := &Error{Status: 400, Retryable: false, Code: code}
		if !IsFallbackWorthy(err) {
			t.Errorf("code %q must be fallback-worthy even though Retryable=false", code)
		}
	}
}

func TestIsFallbackWorthyUnknownCode(t *testing.T) {
	err := &Error{Status: 400, Retryable: false, Code: ""}
	if IsFallbackWorthy(err) {
		t.Error("a non-retryable error with no recognized code must not be fallback-worthy")
	}
	err2 := &Error{Status: 400, Retryable: false, Code: "some_other_code"}
	if IsFallbackWorthy(err2) {
		t.Error("a non-retryable error with an unrecognized code must not be fallback-worthy")
	}
}

func TestIsFallbackWorthyNonProviderError(t *testing.T) {
	if IsFallbackWorthy(errors.New("plain error")) {
		t.Error("a plain non-*Error must not be fallback-worthy, matching IsRetryable's existing behavior")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd /home/roman220/gh/ipsupport-airllm && go test ./internal/providers/... -run TestIsFallbackWorthy -v`
Expected: FAIL — `IsFallbackWorthy`, `ErrCodeContextLengthExceeded`, `ErrCodeModelNotFound`, and `Error.Code` don't exist yet (compile error).

- [ ] **Step 3: Implement**

Replace the full content of `internal/providers/errors.go` with:

```go
package providers

import "errors"

// Known error codes that make a failure fallback-worthy even though it is
// not retryable against the same target — see IsFallbackWorthy.
const (
	ErrCodeContextLengthExceeded = "context_length_exceeded"
	ErrCodeModelNotFound         = "model_not_found"
)

// Error is a provider call failure. Retryable failures (e.g. upstream 429 or
// 5xx) let the router fall back to the next target; non-retryable failures
// (e.g. a bad request) abort — UNLESS Code names a known fallback-worthy
// reason (see IsFallbackWorthy), in which case the router still tries the
// next tier even though retrying the SAME target would not help.
type Error struct {
	Status    int
	Retryable bool
	Code      string
	Message   string
}

func (e *Error) Error() string { return e.Message }

// IsRetryable reports whether err is a retryable provider Error.
func IsRetryable(err error) bool {
	var pe *Error
	if errors.As(err, &pe) {
		return pe.Retryable
	}
	return false
}

// IsFallbackWorthy reports whether the router should try the next
// priority tier after this error, rather than aborting the whole request.
// True for every retryable error (unchanged meaning), plus a short list of
// known error codes that mean "this specific target can't serve this
// request" (e.g. its context window is too small, or its model was
// removed) rather than "this request is malformed."
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

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/providers/... -run TestIsFallbackWorthy -v`
Expected: PASS (4 tests)

- [ ] **Step 5: Run the full provider package suite (nothing else should break)**

Run: `go build ./... && go vet ./... && gofmt -l . && go test ./internal/providers/... -v`
Expected: all green — `Error{Status: ..., Retryable: ..., Message: ...}` struct literals elsewhere in the package (e.g. `openai_compat.go`, `mock.go`) still compile unchanged since `Code` is a new field with a valid zero value (`""`), not required in any literal.

- [ ] **Step 6: Commit**

```bash
git add internal/providers/errors.go internal/providers/errors_test.go
git commit -m "feat(providers): Code field + IsFallbackWorthy for target-specific non-retryable errors"
```

---

### Task 2: `httpError` body parsing — OpenAI-style and llama.cpp/Ollama-style

**Files:**
- Modify: `internal/providers/openai_compat.go:67-73` (the `httpError` function only)
- Test: `internal/providers/openai_compat_test.go` (extend existing file)

**Interfaces:**
- Consumes: `providers.ErrCodeContextLengthExceeded`, `providers.ErrCodeModelNotFound` (Task 1)
- Produces: `httpError`'s returned `*Error` now has `Code` populated when the body matches a known shape (used directly by every existing `httpError`/`audioHTTPError` call site — no call site changes needed, this task only changes `httpError`'s own body)

- [ ] **Step 1: Write the failing tests**

Add to `internal/providers/openai_compat_test.go` (same `package providers`, reuses the file's existing imports — `encoding/json` is not yet imported there; check the current import block and add `encoding/json` if it's missing before writing byte literals with `json.Marshal`, or just write the raw JSON as string literals directly, which needs no new import):

```go
func TestHTTPErrorParsesOpenAIStyleContextLengthExceeded(t *testing.T) {
	body := []byte(`{"error":{"message":"This model's maximum context length is 8192 tokens.","type":"invalid_request_error","code":"context_length_exceeded"}}`)
	err := httpError("openai", 400, body)
	pe, ok := err.(*Error)
	if !ok {
		t.Fatalf("want *Error, got %T", err)
	}
	if pe.Code != ErrCodeContextLengthExceeded {
		t.Errorf("Code = %q, want %q", pe.Code, ErrCodeContextLengthExceeded)
	}
	if pe.Retryable {
		t.Error("a 400 must still be Retryable=false — Code is additive, not a replacement")
	}
}

func TestHTTPErrorParsesOpenAIStyleModelNotFound(t *testing.T) {
	body := []byte(`{"error":{"message":"The model does not exist","type":"invalid_request_error","code":"model_not_found"}}`)
	err := httpError("groq", 404, body)
	pe := err.(*Error)
	if pe.Code != ErrCodeModelNotFound {
		t.Errorf("Code = %q, want %q", pe.Code, ErrCodeModelNotFound)
	}
}

func TestHTTPErrorParsesLlamaCppStyleContextSizeExceeded(t *testing.T) {
	body := []byte(`{"error":{"code":400,"message":"the request exceeds the available context size, try increasing it","type":"exceed_context_size_error","n_prompt_tokens":94520,"n_ctx":8192}}`)
	err := httpError("ollama-local", 400, body)
	pe := err.(*Error)
	if pe.Code != ErrCodeContextLengthExceeded {
		t.Errorf("Code = %q, want %q", pe.Code, ErrCodeContextLengthExceeded)
	}
}

func TestHTTPErrorUnrecognizedBodyLeavesCodeEmpty(t *testing.T) {
	cases := [][]byte{
		[]byte(`{"error":{"message":"bad request","type":"invalid_request_error","code":"something_else"}}`),
		[]byte(`{"error":"model 'x' not found, try pulling it first"}`), // Ollama's plain-string shape, not object
		[]byte(`not even json`),
		[]byte(``),
	}
	for _, body := range cases {
		err := httpError("x", 400, body)
		pe := err.(*Error)
		if pe.Code != "" {
			t.Errorf("body %q: Code = %q, want empty", body, pe.Code)
		}
	}
}

func TestHTTPErrorRetryableUnaffectedByCode(t *testing.T) {
	body := []byte(`{"error":{"message":"rate limited","type":"rate_limit_error"}}`)
	err := httpError("x", 429, body).(*Error)
	if !err.Retryable {
		t.Error("429 must remain Retryable=true regardless of Code parsing")
	}
	if err.Code != "" {
		t.Errorf("this body has no recognized code, want empty, got %q", err.Code)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/providers/... -run TestHTTPError -v`
Expected: FAIL — every case reports `Code = "", want "context_length_exceeded"` (or the model-not-found equivalent); the "unrecognized" and "retryable unaffected" cases already pass trivially since `Code` is always `""` today (that's fine, they'll keep passing after the implementation too — they exist to prevent a future overly-broad parser from matching things it shouldn't).

- [ ] **Step 3: Implement**

Replace the `httpError` function in `internal/providers/openai_compat.go` (currently lines 67-73) with:

```go
// openAIErrorBody is the error shape OpenAI itself, and every OpenAI-compatible
// cloud vendor this codebase talks to (Groq, xAI, OpenRouter), uses.
type openAIErrorBody struct {
	Error struct {
		Type string `json:"type"`
		Code string `json:"code"`
	} `json:"error"`
}

// llamaCppErrorBody is the error shape llama.cpp's server (and Ollama, which
// wraps it) uses — distinct from the OpenAI shape: the reason lives in
// error.type, not error.code, and there is no error.code field at all.
type llamaCppErrorBody struct {
	Error struct {
		Type string `json:"type"`
	} `json:"error"`
}

// classifyErrorBody does best-effort parsing of a non-2xx response body
// against the two known vendor error shapes, returning a recognized
// providers error code or "" if neither shape matches or matched to
// something we don't specifically track. A body that fails to unmarshal
// (malformed JSON, a field of the wrong type such as a null "code") is
// treated exactly like a non-matching body — this is deliberately not
// hardened against every possible shape; the two codes this function
// recognizes always arrive as non-null strings in practice.
func classifyErrorBody(body []byte) string {
	var oa openAIErrorBody
	if err := json.Unmarshal(body, &oa); err == nil {
		switch oa.Error.Code {
		case ErrCodeContextLengthExceeded, ErrCodeModelNotFound:
			return oa.Error.Code
		}
	}
	var lc llamaCppErrorBody
	if err := json.Unmarshal(body, &lc); err == nil {
		if lc.Error.Type == "exceed_context_size_error" {
			return ErrCodeContextLengthExceeded
		}
	}
	return ""
}

func httpError(name string, status int, body []byte) error {
	return &Error{
		Status:    status,
		Retryable: status == http.StatusTooManyRequests || status >= 500,
		Code:      classifyErrorBody(body),
		Message:   fmt.Sprintf("upstream %s returned %d: %s", name, status, strings.TrimSpace(string(body))),
	}
}
```

`encoding/json` is already imported in `openai_compat.go` (used elsewhere in the file for request/response bodies) — no import changes needed in that file.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/providers/... -run TestHTTPError -v`
Expected: PASS (5 tests)

- [ ] **Step 5: Run the full provider package suite**

Run: `go build ./... && go vet ./... && gofmt -l . && go test ./internal/providers/... -v`
Expected: all green, including the existing `audioHTTPError` tests (`openai_compat_transcribe_test.go`) — `audioHTTPError` calls `httpError` internally and only appends to `.Message` on a 404, so its behavior is unaffected by `Code` being newly populated.

- [ ] **Step 6: Commit**

```bash
git add internal/providers/openai_compat.go internal/providers/openai_compat_test.go
git commit -m "feat(providers): recognize context_length_exceeded/model_not_found in httpError"
```

---

### Task 3: Mock `ctxfail` trigger

**Files:**
- Modify: `internal/providers/mock.go` (the `Chat` and `ChatStream` methods)
- Test: `internal/providers/mock_test.go` (extend existing file)

**Interfaces:**
- Consumes: `providers.ErrCodeContextLengthExceeded` (Task 1)
- Produces: a model name containing the substring `"ctxfail"` makes `Mock.Chat`/`Mock.ChatStream` return a non-retryable, `ErrCodeContextLengthExceeded`-coded error — consumed by Task 4's `runChat`/`runStream` fallback tests.

- [ ] **Step 1: Write the failing test**

Add to `internal/providers/mock_test.go` (same file, same imports already present):

```go
func TestMockCtxFailFallbackWorthy(t *testing.T) {
	m := NewMock("mock")

	ctxFailReq := llm.ChatRequest{Model: "mock-ctxfail", Messages: []llm.Message{{Role: "user", Content: "x"}}}
	_, err := m.Chat(context.Background(), ctxFailReq)
	if err == nil {
		t.Fatal("ctxfail model must return an error")
	}
	if IsRetryable(err) {
		t.Error("ctxfail error must NOT be Retryable (it's a target-specific failure, not transient)")
	}
	if !IsFallbackWorthy(err) {
		t.Error("ctxfail error must be fallback-worthy via its Code")
	}

	err = m.ChatStream(context.Background(), ctxFailReq, func(llm.StreamChunk) error {
		t.Fatal("ctxfail model must not yield any chunk")
		return nil
	})
	if !IsFallbackWorthy(err) {
		t.Error("ChatStream ctxfail error must be fallback-worthy via its Code")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/providers/... -run TestMockCtxFail -v`
Expected: FAIL — `Chat`/`ChatStream` don't recognize `"ctxfail"` yet, so this request goes through the normal success path instead of returning an error (`err == nil` fails the first assertion).

- [ ] **Step 3: Implement**

In `internal/providers/mock.go`, `Chat` currently starts:

```go
func (m *Mock) Chat(_ context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
	if strings.Contains(req.Model, "fail") {
		return llm.ChatResponse{}, &Error{Status: 503, Retryable: true, Message: "mock upstream failure for model " + req.Model}
	}
```

Add a new check immediately before the existing `"fail"` check (order matters: `"ctxfail"` also contains the substring `"fail"`, so it must be checked first or the existing branch would shadow it):

```go
func (m *Mock) Chat(_ context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
	if strings.Contains(req.Model, "ctxfail") {
		return llm.ChatResponse{}, &Error{Status: 400, Retryable: false, Code: ErrCodeContextLengthExceeded, Message: "mock upstream context-length error for model " + req.Model}
	}
	if strings.Contains(req.Model, "fail") {
		return llm.ChatResponse{}, &Error{Status: 503, Retryable: true, Message: "mock upstream failure for model " + req.Model}
	}
```

`ChatStream` currently starts:

```go
func (m *Mock) ChatStream(_ context.Context, req llm.ChatRequest, yield func(llm.StreamChunk) error) error {
	if strings.Contains(req.Model, "fail") {
		return &Error{Status: 503, Retryable: true, Message: "mock upstream failure for model " + req.Model}
	}
```

Apply the identical shape of change:

```go
func (m *Mock) ChatStream(_ context.Context, req llm.ChatRequest, yield func(llm.StreamChunk) error) error {
	if strings.Contains(req.Model, "ctxfail") {
		return &Error{Status: 400, Retryable: false, Code: ErrCodeContextLengthExceeded, Message: "mock upstream context-length error for model " + req.Model}
	}
	if strings.Contains(req.Model, "fail") {
		return &Error{Status: 503, Retryable: true, Message: "mock upstream failure for model " + req.Model}
	}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/providers/... -run TestMockCtxFail -v`
Expected: PASS

- [ ] **Step 5: Run the full provider package suite**

Run: `go build ./... && go vet ./... && gofmt -l . && go test ./internal/providers/... -v`
Expected: all green — the existing `TestMockFailRetryable` (plain `"mock-fail"`, no `"ctx"` prefix) is unaffected since `"mock-fail"` does not contain `"ctxfail"`.

- [ ] **Step 6: Commit**

```bash
git add internal/providers/mock.go internal/providers/mock_test.go
git commit -m "feat(providers): Mock ctxfail trigger for fallback-worthy-error testing"
```

---

### Task 4: Wire `IsFallbackWorthy` into `runChat`/`runStream` + `classifyUpstreamErr`

**Files:**
- Modify: `internal/httpapi/exec.go` (two call sites inside `runChat`/`runStream`, plus `classifyUpstreamErr`)
- Test: `internal/httpapi/exec_test.go` (new file)

**Interfaces:**
- Consumes: `providers.IsFallbackWorthy`, `providers.ErrCodeContextLengthExceeded`, `providers.ErrCodeModelNotFound` (Task 1); `Mock`'s `"ctxfail"` trigger (Task 3)

- [ ] **Step 1: Write the failing tests**

Create `internal/httpapi/exec_test.go`:

```go
package httpapi

import (
	"context"
	"net/http"
	"testing"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/llm"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/metrics"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/providers"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/routing"
)

// newRunChatTestServer builds a bare *Server wired only with what
// runChat/runStream touch: the provider registry and the router. No DB,
// no Redis, no HTTP — mirrors dlp_guardrails_test.go's narrow *Server
// construction pattern for the same reason (these are pure-logic tests).
func newRunChatTestServer(t *testing.T, providersToRegister ...providers.Provider) *Server {
	t.Helper()
	reg := providers.NewRegistry()
	for _, p := range providersToRegister {
		reg.Register(p, 0)
	}
	s := &Server{router: routing.NewRouter(nil), metrics: metrics.New()}
	s.regPtr.Store(reg)
	return s
}

func twoTierPlan() *routing.Plan {
	return &routing.Plan{
		Alias:    "text",
		Strategy: "round_robin",
		Tiers: [][]routing.Target{
			{{Provider: "mock-ctxfail", UpstreamModel: "mock-ctxfail-model"}},
			{{Provider: "mock-ok", UpstreamModel: "mock-ok-model"}},
		},
	}
}

func TestRunChatFallsBackOnContextLengthExceeded(t *testing.T) {
	s := newRunChatTestServer(t, providers.NewMock("mock-ctxfail"), providers.NewMock("mock-ok"))
	req := llm.ChatRequest{Messages: []llm.Message{{Role: "user", Content: "hi"}}}

	resp, target, err := s.runChat(context.Background(), twoTierPlan(), req)
	if err != nil {
		t.Fatalf("expected fallback to the second tier to succeed, got error: %v", err)
	}
	if target.Provider != "mock-ok" {
		t.Errorf("target.Provider = %q, want mock-ok (fallback should have been tried)", target.Provider)
	}
	if len(resp.Choices) == 0 {
		t.Error("expected a real response from the fallback tier")
	}
}

func TestRunStreamFallsBackOnContextLengthExceeded(t *testing.T) {
	s := newRunChatTestServer(t, providers.NewMock("mock-ctxfail"), providers.NewMock("mock-ok"))
	req := llm.ChatRequest{Messages: []llm.Message{{Role: "user", Content: "hi"}}}

	var chunks []llm.StreamChunk
	sink := &fakeStreamSink{onChunk: func(c llm.StreamChunk) { chunks = append(chunks, c) }}
	target, _, started, err := s.runStream(context.Background(), twoTierPlan(), req, sink)
	if err != nil {
		t.Fatalf("expected fallback to the second tier to succeed, got error: %v", err)
	}
	if !started {
		t.Error("expected the stream to have started")
	}
	if target.Provider != "mock-ok" {
		t.Errorf("target.Provider = %q, want mock-ok (fallback should have been tried)", target.Provider)
	}
	if len(chunks) == 0 {
		t.Error("expected at least one chunk from the fallback tier")
	}
}

// fakeStreamSink is a minimal streamSink for exec_test.go — records chunks,
// never writes real HTTP output.
type fakeStreamSink struct {
	onChunk func(llm.StreamChunk)
	began   bool
}

func (f *fakeStreamSink) begin(routing.Target) { f.began = true }
func (f *fakeStreamSink) chunk(c llm.StreamChunk) error {
	f.onChunk(c)
	return nil
}

func TestClassifyUpstreamErrContextLengthExceeded(t *testing.T) {
	err := &providers.Error{Status: 400, Retryable: false, Code: providers.ErrCodeContextLengthExceeded, Message: "too big"}
	code, typ := classifyUpstreamErr(err)
	if code != http.StatusBadRequest || typ != "invalid_request_error" {
		t.Errorf("got (%d, %q), want (400, invalid_request_error)", code, typ)
	}
}

func TestClassifyUpstreamErrModelNotFound(t *testing.T) {
	err := &providers.Error{Status: 404, Retryable: false, Code: providers.ErrCodeModelNotFound, Message: "gone"}
	code, typ := classifyUpstreamErr(err)
	if code != http.StatusBadRequest || typ != "invalid_request_error" {
		t.Errorf("got (%d, %q), want (400, invalid_request_error)", code, typ)
	}
}

func TestClassifyUpstreamErrUnrecognizedStaysUpstreamError(t *testing.T) {
	err := &providers.Error{Status: 400, Retryable: false, Message: "malformed"}
	code, typ := classifyUpstreamErr(err)
	if code != http.StatusBadGateway || typ != "upstream_error" {
		t.Errorf("got (%d, %q), want (502, upstream_error) — unrecognized errors must keep today's mapping", code, typ)
	}
}

func TestClassifyUpstreamErrAllBusyUnaffected(t *testing.T) {
	code, typ := classifyUpstreamErr(errAllBusy)
	if code != http.StatusTooManyRequests || typ != "rate_limit_error" {
		t.Errorf("got (%d, %q), want (429, rate_limit_error) — errAllBusy mapping must be untouched", code, typ)
	}
}
```

Confirmed against `exec.go:187-190`: `streamSink` is exactly the two-method interface `fakeStreamSink` above implements (`begin(t routing.Target)`, `chunk(llm.StreamChunk) error`) — no other methods to add.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/httpapi/... -run 'TestRunChatFallsBack|TestRunStreamFallsBack|TestClassifyUpstreamErr' -v`
Expected: FAIL — `TestRunChatFallsBackOnContextLengthExceeded` and `TestRunStreamFallsBackOnContextLengthExceeded` fail with an error returned instead of success (today's code aborts on the ctxfail tier); `TestClassifyUpstreamErrContextLengthExceeded` and `TestClassifyUpstreamErrModelNotFound` fail with `(502, upstream_error)` instead of `(400, invalid_request_error)`; `TestClassifyUpstreamErrUnrecognizedStaysUpstreamError` and `TestClassifyUpstreamErrAllBusyUnaffected` already pass (they assert today's existing behavior, kept as regression guards).

- [ ] **Step 3: Implement**

In `internal/httpapi/exec.go`, inside `runChat`, the existing block:

```go
		resp, err := e.Provider.Chat(ctx, upstreamRequest(req, t.UpstreamModel))
		e.Release()
		if err == nil {
			return resp, t, nil
		}
		lastErr = err
		if !providers.IsRetryable(err) {
			return llm.ChatResponse{}, t, err
		}
```

becomes (only the condition changes):

```go
		resp, err := e.Provider.Chat(ctx, upstreamRequest(req, t.UpstreamModel))
		e.Release()
		if err == nil {
			return resp, t, nil
		}
		lastErr = err
		if !providers.IsFallbackWorthy(err) {
			return llm.ChatResponse{}, t, err
		}
```

Inside `runStream`, the existing block:

```go
			if callErr == nil {
				return t, attemptUsage, true, nil
			}
			lastErr = callErr
			if attemptStarted {
				return t, attemptUsage, true, callErr
			}
			if !providers.IsRetryable(callErr) {
				return t, llm.Usage{}, false, callErr
			}
```

becomes (only the last condition changes — the `attemptStarted` check stays first and unchanged, since a stream that already began can never fall back regardless of error classification):

```go
			if callErr == nil {
				return t, attemptUsage, true, nil
			}
			lastErr = callErr
			if attemptStarted {
				return t, attemptUsage, true, callErr
			}
			if !providers.IsFallbackWorthy(callErr) {
				return t, llm.Usage{}, false, callErr
			}
```

`classifyUpstreamErr` currently:

```go
// classifyUpstreamErr maps an executor error to an HTTP status: all-busy is a
// 429 (back off and retry), anything else is a 502 upstream error.
func classifyUpstreamErr(err error) (int, string) {
	if errors.Is(err, errAllBusy) {
		return http.StatusTooManyRequests, "rate_limit_error"
	}
	return http.StatusBadGateway, "upstream_error"
}
```

becomes:

```go
// classifyUpstreamErr maps an executor error to an HTTP status: all-busy is a
// 429 (back off and retry); a recognized context-length/model-not-found
// error that still failed on every fallback tier is a 400 the client can
// act on; anything else is a 502 upstream error.
func classifyUpstreamErr(err error) (int, string) {
	if errors.Is(err, errAllBusy) {
		return http.StatusTooManyRequests, "rate_limit_error"
	}
	var pe *providers.Error
	if errors.As(err, &pe) {
		switch pe.Code {
		case providers.ErrCodeContextLengthExceeded, providers.ErrCodeModelNotFound:
			return http.StatusBadRequest, "invalid_request_error"
		}
	}
	return http.StatusBadGateway, "upstream_error"
}
```

`errors` and `providers` are already imported in `exec.go` (both used elsewhere in the file already) — no import changes needed there.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/httpapi/... -run 'TestRunChatFallsBack|TestRunStreamFallsBack|TestClassifyUpstreamErr' -v`
Expected: PASS (6 tests)

- [ ] **Step 5: Run the full suite**

Run: `gofmt -l . && go build ./... && go vet ./... && go test ./...`
Expected: all green, every existing test in `internal/httpapi` and `internal/providers` still passes.

- [ ] **Step 6: Commit**

```bash
git add internal/httpapi/exec.go internal/httpapi/exec_test.go
git commit -m "feat(httpapi): fall back to the next tier on context-length/model-not-found errors"
```

---

## Final verification (controller, after all 4 tasks)

- [ ] `gofmt -l . && go build ./... && go vet ./... && go test ./...` — all green.
- [ ] Live sanity check against the dev docker-compose stack: configure a two-tier alias (`mock` provider target 1 named e.g. `ctxfail-demo`, if the Mock provider's registered name in dev can be aliased to trigger the model-name substring match via the alias's `upstream_model` field — the trigger is on `req.Model` after `upstreamRequest(req, t.UpstreamModel)` substitutes in the target's `UpstreamModel`, so set that target's upstream model to something containing `"ctxfail"`; tier 2 = ordinary `mock-gpt`) and confirm a chat completion against that alias succeeds via tier 2, not a 400.
- [ ] Confirm `docs/superpowers/specs/2026-09-01-fallback-worthy-errors-design.md`'s every section maps to a task above (spec coverage check — Task 1 = Error taxonomy section, Task 2 = httpError parsing section, Task 3 = Mock testing section, Task 4 = runChat/runStream/classifyUpstreamErr section — all covered).
