# OpenAI Request-Field Passthrough Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Unmapped OpenAI request fields pass through the gateway verbatim (`Extra` map on the IR), with `n ≠ 1` rejected instead of silently corrupting.

**Architecture:** `DecodeChatRequest` splits the body into owned struct fields + leftover raw keys; `EncodeChatRequest` merges the leftovers back into the upstream body. One task.

**Tech Stack:** Go 1.26. No new deps. No migration.

**Spec:** `docs/superpowers/specs/2026-07-14-openai-field-passthrough-design.md`

## Global Constraints

- English only; no new Go dependencies.
- Owned keys (never in `Extra`): `model`, `messages`, `tools`, `tool_choice`, `temperature`, `max_tokens`, `stream`, `parallel_tool_calls`, `stream_options`, `n`.
- `n` present and ≠ 1 → decode returns an error whose message contains "n is not supported" (the existing handler already maps decode errors to 400 `invalid_request_error`).
- A request with no extra fields must encode byte-identically to today (guard against reordering surprises: compare decoded JSON, not raw bytes, where ordering is not guaranteed).
- Anthropic ingress (`internal/anthropic/codec.go`) untouched; `Extra` stays nil there.
- `gofmt -l .` clean; full suite green.

---

### Task 1: Extra map through the OpenAI codec

**Files:**
- Modify: `internal/llm/types.go` (`ChatRequest`)
- Modify: `internal/openai/codec.go` (`DecodeChatRequest`)
- Modify: `internal/openai/client.go` (`EncodeChatRequest`)
- Test: `internal/openai/codec_test.go` (extend or create)

**Interfaces:**
- Produces: `llm.ChatRequest.Extra map[string]json.RawMessage` (nil when no extras).

- [ ] **Step 1: Write the failing tests**

In `internal/openai/codec_test.go` (create if absent, package `openai`):

```go
func TestDecodeChatRequestExtractsExtras(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}],` +
		`"temperature":0.5,"top_p":0.9,"stop":["a","b"],` +
		`"response_format":{"type":"json_object"},"max_completion_tokens":128}`
	req, err := DecodeChatRequest(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if req.Temperature == nil || *req.Temperature != 0.5 {
		t.Errorf("owned field lost: %+v", req.Temperature)
	}
	want := map[string]string{
		"top_p":                 `0.9`,
		"stop":                  `["a","b"]`,
		"response_format":       `{"type":"json_object"}`,
		"max_completion_tokens": `128`,
	}
	if len(req.Extra) != len(want) {
		t.Fatalf("extra keys = %v", req.Extra)
	}
	for k, v := range want {
		if string(req.Extra[k]) != v {
			t.Errorf("extra[%s] = %s, want %s", k, req.Extra[k], v)
		}
	}
}

func TestDecodeChatRequestNoExtras(t *testing.T) {
	req, err := DecodeChatRequest(strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if req.Extra != nil {
		t.Errorf("want nil Extra, got %v", req.Extra)
	}
}

func TestDecodeChatRequestRejectsMultiChoice(t *testing.T) {
	_, err := DecodeChatRequest(strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}],"n":2}`))
	if err == nil || !strings.Contains(err.Error(), "n is not supported") {
		t.Fatalf("want n rejection, got %v", err)
	}
	if _, err := DecodeChatRequest(strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}],"n":1}`)); err != nil {
		t.Fatalf("n:1 must pass, got %v", err)
	}
}
```

In `internal/openai` (same test file):

```go
func TestEncodeChatRequestMergesExtras(t *testing.T) {
	req := llm.ChatRequest{
		Model:    "m",
		Messages: []llm.Message{{Role: "user", Content: "hi"}},
		Extra: map[string]json.RawMessage{
			"top_p":           json.RawMessage(`0.9`),
			"response_format": json.RawMessage(`{"type":"json_object"}`),
		},
	}
	b, err := EncodeChatRequest(req, true)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if string(got["top_p"]) != `0.9` || string(got["response_format"]) != `{"type":"json_object"}` {
		t.Errorf("extras not merged: %s", b)
	}
	if string(got["model"]) != `"m"` || string(got["stream"]) != `true` {
		t.Errorf("owned fields damaged: %s", b)
	}
	if _, ok := got["stream_options"]; !ok {
		t.Errorf("gateway-imposed stream_options lost: %s", b)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/openai/`
Expected: FAIL (`Extra` undefined; extras dropped; `n` accepted).

- [ ] **Step 3: Implement**

`internal/llm/types.go` — `ChatRequest` gains:

```go
	// Extra carries unmapped OpenAI request fields verbatim (OpenAI ingress →
	// OpenAI-compatible upstream). Nil when the request had none.
	Extra map[string]json.RawMessage
```

`internal/openai/codec.go` — `DecodeChatRequest` reads the body once
(`io.ReadAll`), unmarshals into the wire struct as today, then into
`map[string]json.RawMessage`; rejects `n` ≠ 1; deletes owned keys; leftover
map (if non-empty) becomes `Extra`:

```go
var ownedRequestKeys = map[string]bool{
	"model": true, "messages": true, "tools": true, "tool_choice": true,
	"temperature": true, "max_tokens": true, "stream": true,
	"parallel_tool_calls": true, "stream_options": true, "n": true,
}
```

```go
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return llm.ChatRequest{}, err
	}
	if nRaw, ok := raw["n"]; ok && string(nRaw) != "1" && string(nRaw) != "null" {
		return llm.ChatRequest{}, errors.New("n is not supported (single choice only)")
	}
	var extra map[string]json.RawMessage
	for k, v := range raw {
		if !ownedRequestKeys[k] {
			if extra == nil {
				extra = make(map[string]json.RawMessage)
			}
			extra[k] = v
		}
	}
```

`internal/openai/client.go` — `EncodeChatRequest`: marshal the struct as
today; if `req.Extra` is empty return it unchanged; otherwise unmarshal the
marshalled bytes into `map[string]json.RawMessage`, add the extra keys, and
marshal the merged map:

```go
	b, err := json.Marshal(u)
	if err != nil || len(req.Extra) == 0 {
		return b, err
	}
	var merged map[string]json.RawMessage
	if err := json.Unmarshal(b, &merged); err != nil {
		return nil, err
	}
	for k, v := range req.Extra {
		merged[k] = v
	}
	return json.Marshal(merged)
```

- [ ] **Step 4: Run the full suite**

Run: `gofmt -l . && go build ./... && go vet ./... && go test ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/llm/types.go internal/openai/
git commit -m "feat(openai): pass unmapped request fields through verbatim; reject n != 1"
```

---

### Task 2: Live verification + chart 0.1.11 (controller)

- [ ] `deploy/helm/airllm/Chart.yaml` → `version: 0.1.11`, `appVersion: "0.1.11"`; `make helm-lint`.
- [ ] Rebuild dev app; send `top_p`/`stop`/`response_format`/`max_completion_tokens` through an alias to the stub; assert `/last-request` carries them verbatim; `n:2` → 400; a plain request still round-trips.
- [ ] Playwright e2e regression; groq appears in the provider Kind dropdown.
