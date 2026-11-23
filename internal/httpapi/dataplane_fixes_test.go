package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/auth"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/capture"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/dlp"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/llm"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/providers"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/routing"
)

// rawUpstream serves body as an OpenAI-shaped SSE (stream) or JSON (unary)
// chat-completions answer with status 200.
func rawUpstream(t *testing.T, body string) *httptest.Server {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(up.Close)
	return up
}

func flakyFirstPlan() *routing.Plan {
	return &routing.Plan{
		Alias:    "text",
		Strategy: "round_robin",
		Tiers: [][]routing.Target{
			{{Provider: "flaky", UpstreamModel: "m", Tier: 0}},
			{{Provider: "mock-ok", UpstreamModel: "mock-ok-model", Tier: 1}},
		},
	}
}

// TestRunStreamFailsOverOnErrorEnvelopeBeforeCommit: an error envelope that
// arrives before any output is a provider failure like a non-2xx answer —
// the next tier serves the request.
func TestRunStreamFailsOverOnErrorEnvelopeBeforeCommit(t *testing.T) {
	up := rawUpstream(t, "data: {\"error\":{\"message\":\"CUDA out of memory\",\"code\":500}}\n\n")
	s := newRunChatTestServer(t, providers.NewOpenAICompat("flaky", "openai", up.URL, ""), providers.NewMock("mock-ok"))
	sink := &fakeStreamSink{onChunk: func(llm.StreamChunk) {}}
	served, _, started, err := s.runStream(context.Background(), flakyFirstPlan(), llm.ChatRequest{Messages: []llm.Message{{Role: "user", Content: "hi"}}}, sink)
	if err != nil || !started {
		t.Fatalf("want the next tier to serve, got started=%v err=%v", started, err)
	}
	if served.Provider != "mock-ok" {
		t.Errorf("served by %q, want mock-ok", served.Provider)
	}
}

// TestRunStreamErrorEnvelopeAfterCommitIsAnError: once output went out the
// error cannot fail over, but it must surface as an error, not a clean end.
func TestRunStreamErrorEnvelopeAfterCommitIsAnError(t *testing.T) {
	up := rawUpstream(t, "data: {\"choices\":[{\"delta\":{\"content\":\"par\"}}]}\n\ndata: {\"error\":{\"message\":\"died\"}}\n\n")
	s := newRunChatTestServer(t, providers.NewOpenAICompat("flaky", "openai", up.URL, ""), providers.NewMock("mock-ok"))
	var finish string
	sink := &fakeStreamSink{onChunk: func(c llm.StreamChunk) {
		if c.FinishReason != "" {
			finish = c.FinishReason
		}
	}}
	_, _, started, err := s.runStream(context.Background(), flakyFirstPlan(), llm.ChatRequest{Messages: []llm.Message{{Role: "user", Content: "hi"}}}, sink)
	if err == nil || !started {
		t.Fatalf("want a committed error, got started=%v err=%v", started, err)
	}
	if finish != "" {
		t.Errorf("no finish reason may reach the client of a failed stream, got %q", finish)
	}
}

// TestFailCommittedStreamOpenAI: a committed stream that fails ends with an
// OpenAI error event and no [DONE], and is recorded with the classified
// status; a client that went away gets nothing written and a 200.
func TestFailCommittedStreamOpenAI(t *testing.T) {
	upErr := &providers.Error{Status: 502, Retryable: true, Message: "upstream flaky returned 502: secret detail", VendorMessage: "died"}
	plan := &routing.Plan{}

	rec := httptest.NewRecorder()
	sink := &openaiSink{w: rec, flush: rec.Flush}
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	code := failCommittedStream(r, upErr, plan, func(_ int, typ, msg string) { sink.fail(typ, msg) })
	if code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", code)
	}
	body := rec.Body.String()
	if !strings.HasPrefix(body, "data: {\"error\":") || strings.Contains(body, "[DONE]") {
		t.Errorf("want one error event and no [DONE], got %q", body)
	}
	if strings.Contains(body, "secret detail") || !strings.Contains(body, "died") {
		t.Errorf("client must get the vendor message only, got %q", body)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	code = failCommittedStream(r.WithContext(ctx), upErr, plan, func(int, string, string) { called = true })
	if code != http.StatusOK || called {
		t.Errorf("client gone: want 200 and nothing written, got %d called=%v", code, called)
	}
}

// TestRunChatEmptyChoicesFailsOver: a 2xx answer without choices (an error
// body sent with a success status) is not an answer.
func TestRunChatEmptyChoicesFailsOver(t *testing.T) {
	up := rawUpstream(t, `{"error":{"message":"oops"}}`)
	s := newRunChatTestServer(t, providers.NewOpenAICompat("flaky", "openai", up.URL, ""), providers.NewMock("mock-ok"))
	resp, served, err := s.runChat(context.Background(), flakyFirstPlan(), llm.ChatRequest{Messages: []llm.Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("want fallback, got %v", err)
	}
	if served.Provider != "mock-ok" || len(resp.Choices) == 0 {
		t.Errorf("served=%q choices=%d", served.Provider, len(resp.Choices))
	}

	only := &routing.Plan{Alias: "text", Strategy: "round_robin", Tiers: [][]routing.Target{{{Provider: "flaky", UpstreamModel: "m"}}}}
	_, _, err = s.runChat(context.Background(), only, llm.ChatRequest{Messages: []llm.Message{{Role: "user", Content: "hi"}}})
	if code, _ := classifyUpstreamErr(err); code != http.StatusBadGateway {
		t.Errorf("no choices anywhere: want 502, got %d (%v)", code, err)
	}
}

func TestClientErrorMessage(t *testing.T) {
	vertex := &providers.Error{
		Status:        400,
		Message:       "upstream vertex-prod returned 400: {raw body}",
		VendorMessage: "Publisher model projects/acme-prod-123/locations/us/publishers/google/models/x not found, see https://cloud.google.com/vertex",
	}
	got := clientErrorMessage(vertex, false)
	for _, leak := range []string{"vertex-prod", "acme-prod-123", "https://", "raw body"} {
		if strings.Contains(got, leak) {
			t.Errorf("client message leaks %q: %q", leak, got)
		}
	}
	if !strings.Contains(got, "not found") {
		t.Errorf("vendor message lost: %q", got)
	}
	if clientErrorMessage(vertex, true) != vertex.Message {
		t.Error("an exposed backend gets the full detail")
	}
	transport := &providers.Error{Status: 502, Retryable: true, Message: `Post "http://10.0.0.5:8080/v1/chat/completions": connection refused`}
	if got := clientErrorMessage(transport, false); strings.Contains(got, "10.0.0.5") {
		t.Errorf("transport error leaks the base URL: %q", got)
	}
	timeout := &providers.Error{Status: 504, Code: errCodeTierTimeout, Message: "upstream secret-provider did not answer within 5s"}
	if got := clientErrorMessage(timeout, false); strings.Contains(got, "secret-provider") {
		t.Errorf("timeout leaks the provider: %q", got)
	}
	if got := clientErrorMessage(errAllBusy, false); got != errAllBusy.Error() {
		t.Errorf("gateway's own message must pass: %q", got)
	}
	if got := clientErrorMessage(errors.New(`provider "internal-x" not registered`), false); strings.Contains(got, "internal-x") {
		t.Errorf("unknown error leaks: %q", got)
	}
}

const toolSecret = "sk-ant-api03-aaaabbbbccccddddeeee1234"

func toolCallRequest() llm.ChatRequest {
	return llm.ChatRequest{Model: "m", Messages: []llm.Message{
		{Role: "user", Content: "deploy"},
		{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "c1", Type: "function", Function: llm.FunctionCall{
			Name: "bash", Arguments: `{"cmd":"export KEY=` + toolSecret + `","n":1}`,
		}}}},
	}}
}

// TestDLPEnforceRedactsToolCallArguments: tool-call arguments go upstream
// like content does, so they are scanned and, in redact mode, masked with
// the JSON kept valid; the raw-window snapshot keeps the original.
func TestDLPEnforceRedactsToolCallArguments(t *testing.T) {
	s := newDLPScanTextTestServer(t, dlpConfig{Enabled: true, Action: "redact"})
	req := toolCallRequest()
	blocked, _, res := s.dlpEnforce(context.Background(), authedKey{}, "openai", &req, false)
	if blocked || !res.HadIncident {
		t.Fatalf("blocked=%v incident=%v", blocked, res.HadIncident)
	}
	args := req.Messages[1].ToolCalls[0].Function.Arguments
	if strings.Contains(args, toolSecret) || !strings.Contains(args, "[REDACTED:") {
		t.Errorf("arguments not masked: %s", args)
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(args), &v); err != nil {
		t.Errorf("masked arguments are not valid JSON: %v: %s", err, args)
	}
	if !strings.Contains(res.OriginalMessages[1].ToolCalls[0].Function.Arguments, toolSecret) {
		t.Error("the pre-redaction snapshot must keep the original arguments")
	}
}

// TestDLPEnforceRecordsToolCallFindings: a secret in tool-call arguments is
// recorded in the capture's flat Findings, located by tool call with no
// offsets, and kept out of the per-message content findings (whose offsets
// redact Content).
func TestDLPEnforceRecordsToolCallFindings(t *testing.T) {
	s := newDLPScanTextTestServer(t, dlpConfig{Enabled: true, Action: "flag"})
	req := toolCallRequest()
	_, _, res := s.dlpEnforce(context.Background(), authedKey{}, "openai", &req, false)
	if len(res.Findings) != 1 {
		t.Fatalf("want 1 finding, got %+v", res.Findings)
	}
	f := res.Findings[0]
	want := dlp.ToolCallRef{Message: 1, Index: 0, ID: "c1"}
	if f.ToolCall == nil || *f.ToolCall != want || f.Start != 0 || f.End != 0 || f.Label == "" {
		t.Errorf("finding = %+v (ref %+v), want label, zero offsets, ref %+v", f, f.ToolCall, want)
	}
	for i, fs := range res.MsgFindings {
		if len(fs) != 0 {
			t.Errorf("MsgFindings[%d] = %+v, want none (tool-call findings index no content)", i, fs)
		}
	}
	if len(dlp.ContentFindings(res.Findings)) != 0 {
		t.Error("a tool-call finding must not count as a content finding")
	}
}

func TestDLPEnforceBlocksOnToolCallArguments(t *testing.T) {
	s := newDLPScanTextTestServer(t, dlpConfig{Enabled: true, Action: "block"})
	req := toolCallRequest()
	if blocked, _, _ := s.dlpEnforce(context.Background(), authedKey{}, "openai", &req, false); !blocked {
		t.Error("a secret in tool-call arguments must block")
	}
}

// TestRedactForCaptureWithoutEnforcement: a capture stored as redacted must
// not hold a secret just because DLP enforcement was off (no findings to
// reuse) — content and tool-call arguments are scanned for the capture.
func TestRedactForCaptureWithoutEnforcement(t *testing.T) {
	msgs := toolCallRequest().Messages
	msgs[0].Content = "my key " + toolSecret
	out := redactForCapture(msgs, nil, layer1Patterns(dlpConfig{}))
	b, _ := json.Marshal(out)
	if strings.Contains(string(b), toolSecret) {
		t.Errorf("capture body holds the raw secret: %s", b)
	}
	if !strings.Contains(msgs[0].Content, toolSecret) || !strings.Contains(msgs[1].ToolCalls[0].Function.Arguments, toolSecret) {
		t.Error("redactForCapture must not modify the request's messages")
	}
	// With enforcement findings, content uses them (no rescan) and tool
	// arguments are still masked.
	withFindings := redactForCapture(msgs, [][]dlp.Finding{{{Label: "anthropic_key", Start: 7, End: 7 + len(toolSecret)}}, nil}, layer1Patterns(dlpConfig{}))
	b, _ = json.Marshal(withFindings)
	if strings.Contains(string(b), toolSecret) {
		t.Errorf("capture body holds the raw secret: %s", b)
	}
}

// TestAuditGetCaptureBlobErrorIs500: an unreadable body is an error, not an
// empty transcript.
func TestAuditGetCaptureBlobErrorIs500(t *testing.T) {
	store := &fakeCaptureStore{rows: []capture.IndexRow{{ID: "row1", BlobKey: "captures/missing", ReviewStatus: "unreviewed"}}}
	auditor := auth.Principal{Subject: "auditor1", Roles: []string{auth.AuditorRole}}
	srv, _ := newAuditTestServer(t, auditor, store, newFakeMemBlob())
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest("GET", "/api/audit/captures/row1", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d: %s", rec.Code, rec.Body.String())
	}
}
