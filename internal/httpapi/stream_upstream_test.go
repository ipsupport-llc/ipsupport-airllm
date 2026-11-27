package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/anthropic"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/llm"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/openai"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/providers"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/routing"
)

// These tests pin stream_upstream: a unary chat served from a stream and
// assembled into the response a unary call would have given, with the
// budget bounding the first output and the stall timeout every gap after
// it.

// sseStep is one step of a scripted upstream stream: wait, then send data.
// hang holds the connection open without sending anything more until the
// caller goes away; cut drops it mid-body.
type sseStep struct {
	wait time.Duration
	data string
	hang bool
	cut  bool
}

// scriptedUpstream answers a non-streaming request with unary and a
// streaming one with steps, counting each kind and keeping the last body.
type scriptedUpstream struct {
	*httptest.Server
	unary       string
	steps       []sseStep
	unaryCalls  atomic.Int32
	streamCalls atomic.Int32
	mu          sync.Mutex
	lastBody    string
}

func newScriptedUpstream(t *testing.T, unary string, steps []sseStep) *scriptedUpstream {
	t.Helper()
	u := &scriptedUpstream{unary: unary, steps: steps}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.lastBody = string(body)
		u.mu.Unlock()
		if !strings.Contains(string(body), `"stream":true`) {
			u.unaryCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, u.unary)
			return
		}
		u.streamCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		flush := w.(http.Flusher).Flush
		flush()
		for _, st := range u.steps {
			if st.wait > 0 {
				select {
				case <-r.Context().Done():
					return
				case <-time.After(st.wait):
				}
			}
			switch {
			case st.hang:
				<-r.Context().Done()
				return
			case st.cut:
				panic(http.ErrAbortHandler)
			}
			_, _ = fmt.Fprintf(w, "data: %s\n\n", st.data)
			flush()
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *scriptedUpstream) body() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.lastBody
}

// The steps below build streams in the shape llama.cpp sends them.
func roleStep() sseStep {
	return sseStep{data: `{"choices":[{"index":0,"delta":{"role":"assistant","content":null},"finish_reason":null}]}`}
}

func contentStep(wait time.Duration, text string) sseStep {
	b, _ := json.Marshal(text)
	return sseStep{wait: wait, data: `{"choices":[{"index":0,"delta":{"content":` + string(b) + `},"finish_reason":null}]}`}
}

func reasoningStep(wait time.Duration, text string) sseStep {
	b, _ := json.Marshal(text)
	return sseStep{wait: wait, data: `{"choices":[{"index":0,"delta":{"reasoning_content":` + string(b) + `},"finish_reason":null}]}`}
}

func finishStep(reason string) sseStep {
	return sseStep{data: `{"choices":[{"index":0,"delta":{},"finish_reason":"` + reason + `"}]}`}
}

func usageStep() sseStep {
	return sseStep{data: `{"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`}
}

const okUnary = `{"id":"c","object":"chat.completion","created":1700000000,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`

// helloSteps is okUnary as a stream.
func helloSteps() []sseStep {
	return []sseStep{roleStep(), contentStep(0, "Hel"), contentStep(0, "lo"), finishStep("stop"), usageStep()}
}

func streamUpstreamOpts() routing.TargetOptions {
	return routing.TargetOptions{StreamUpstream: boolp(true)}
}

func hiRequest() llm.ChatRequest {
	return llm.ChatRequest{Messages: []llm.Message{{Role: "user", Content: "hi"}}}
}

// TestStreamUpstreamMatchesUnary: one upstream serving the same answer both
// ways — thinking, text and two tool calls, each split across chunks, a
// finish reason and usage with reasoning tokens — gives byte for byte the
// same client response either way, apart from the id and created stamp the
// gateway gives an assembled one. With thinking off, both cut the same
// <think> block.
func TestStreamUpstreamMatchesUnary(t *testing.T) {
	cases := []struct {
		name  string
		opts  routing.TargetOptions
		unary string
		steps []sseStep
	}{
		{
			name:  "tool calls and reasoning",
			unary: `{"id":"chatcmpl-up","object":"chat.completion","created":1700000000,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"Hello","reasoning_content":"think hard","tool_calls":[{"id":"call_a","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}},{"id":"call_b","type":"function","function":{"name":"get_time","arguments":"{\"tz\":\"CET\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":12,"completion_tokens":30,"total_tokens":42,"completion_tokens_details":{"reasoning_tokens":7}}}`,
			steps: []sseStep{
				roleStep(),
				reasoningStep(0, "think "),
				reasoningStep(0, "hard"),
				contentStep(0, "Hel"),
				contentStep(0, "lo"),
				{data: `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"get_weather","arguments":""}}]},"finish_reason":null}]}`},
				{data: `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]},"finish_reason":null}]}`},
				{data: `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"get_time","arguments":"{\"tz\":"}}]},"finish_reason":null}]}`},
				{data: `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Paris\"}"}}]},"finish_reason":null}]}`},
				{data: `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"\"CET\"}"}}]},"finish_reason":null}]}`},
				finishStep("tool_calls"),
				{data: `{"choices":[],"usage":{"prompt_tokens":12,"completion_tokens":30,"total_tokens":42,"completion_tokens_details":{"reasoning_tokens":7}}}`},
			},
		},
		{
			name:  "thinking off",
			opts:  routing.TargetOptions{Thinking: strp("off")},
			unary: `{"id":"chatcmpl-up","object":"chat.completion","created":1700000000,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"<think>a</think>Hi"},"finish_reason":"length"}],"usage":{"prompt_tokens":3,"completion_tokens":9,"total_tokens":12}}`,
			steps: []sseStep{roleStep(), contentStep(0, "<thi"), contentStep(0, "nk>a</think>H"), contentStep(0, "i"), finishStep("length"),
				{data: `{"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":9,"total_tokens":12}}`}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := newScriptedUpstream(t, tc.unary, tc.steps)
			s := newRunChatTestServer(t, providers.NewOpenAICompat("slow", "openai", up.URL, ""))
			req := hiRequest()
			req.Tools = []llm.Tool{{Type: "function", Function: llm.FunctionDef{Name: "get_weather"}}}

			unary, _, err := s.runChat(context.Background(), soloPlan(tc.opts), req)
			if err != nil {
				t.Fatalf("unary: %v", err)
			}
			if up.unaryCalls.Load() != 1 || up.streamCalls.Load() != 0 {
				t.Fatalf("without stream_upstream: %d unary / %d stream calls, want 1 / 0", up.unaryCalls.Load(), up.streamCalls.Load())
			}
			opts := tc.opts
			opts.StreamUpstream = boolp(true)
			streamed, _, err := s.runChat(context.Background(), soloPlan(opts), req)
			if err != nil {
				t.Fatalf("stream_upstream: %v", err)
			}
			if up.streamCalls.Load() != 1 {
				t.Fatalf("stream_upstream made %d stream calls, want 1", up.streamCalls.Load())
			}
			if b := up.body(); !strings.Contains(b, `"stream_options":{"include_usage":true}`) {
				t.Errorf("the stream request does not ask for usage: %s", b)
			}
			if !strings.HasPrefix(streamed.ID, "chatcmpl-") || streamed.Created == 0 {
				t.Errorf("assembled response id %q created %d, want a chatcmpl- id and a timestamp", streamed.ID, streamed.Created)
			}
			streamed.ID, streamed.Created = unary.ID, unary.Created
			want, _ := openai.MarshalChatResponse(unary)
			got, _ := openai.MarshalChatResponse(streamed)
			if string(got) != string(want) {
				t.Errorf("assembled response differs from the unary one:\n got %s\nwant %s", got, want)
			}
		})
	}
}

func strp(s string) *string { return &s }

// TestStreamUpstreamStallFallsThrough: a tier that starts answering and then
// goes quiet past its stall timeout is abandoned and the next tier serves.
func TestStreamUpstreamStallFallsThrough(t *testing.T) {
	up := newScriptedUpstream(t, okUnary, []sseStep{roleStep(), contentStep(0, "Hel"), {hang: true}})
	s := newUnavailTestServer(t, newFakeClock(), providers.NewOpenAICompat("slow", "openai", up.URL, ""), providers.NewMock("mock-ok"))
	plan := budgetPlan(routing.Target{Provider: "slow", UpstreamModel: "m", Options: routing.TargetOptions{
		StreamUpstream: boolp(true), TimeoutMS: ms(5000), StallTimeoutMS: ms(50),
	}})

	began := time.Now()
	resp, res, err := s.runChat(context.Background(), plan, hiRequest())
	s.bg.Wait()
	if err != nil || res.Provider != "mock-ok" || res.Attempts != 2 {
		t.Fatalf("served by %q after %d attempts, err %v; want mock-ok after 2", res.Provider, res.Attempts, err)
	}
	if len(resp.Choices) != 1 || !strings.Contains(resp.Choices[0].Message.Content, "mock-ok") {
		t.Errorf("response = %+v, want only the second tier's answer", resp)
	}
	if d := time.Since(began); d > 3*time.Second {
		t.Errorf("took %v: the stall timeout, not the 5s budget, should have ended the attempt", d)
	}
}

// TestStreamUpstreamLongPrefillIsNotAStall: a role chunk and empty
// keep-alive deltas while the prompt is read do not start the stall clock,
// and thinking keeps it running; the budget is what bounds the first
// output.
func TestStreamUpstreamLongPrefillIsNotAStall(t *testing.T) {
	cases := map[string][]sseStep{
		"prefill": {roleStep(), {wait: 40 * time.Millisecond, data: `{"choices":[{"index":0,"delta":{},"finish_reason":null}]}`},
			contentStep(200*time.Millisecond, "Hel"), contentStep(0, "lo"), finishStep("stop"), usageStep()},
		"thinking": {roleStep(), reasoningStep(30*time.Millisecond, "a"), reasoningStep(30*time.Millisecond, "b"),
			reasoningStep(30*time.Millisecond, "c"), reasoningStep(30*time.Millisecond, "d"),
			contentStep(30*time.Millisecond, "Hel"), contentStep(0, "lo"), finishStep("stop"), usageStep()},
	}
	for name, steps := range cases {
		t.Run(name, func(t *testing.T) {
			up := newScriptedUpstream(t, okUnary, steps)
			s := newRunChatTestServer(t, providers.NewOpenAICompat("slow", "openai", up.URL, ""))
			opts := routing.TargetOptions{StreamUpstream: boolp(true), TimeoutMS: ms(5000), StallTimeoutMS: ms(100)}
			resp, _, err := s.runChat(context.Background(), soloPlan(opts), hiRequest())
			if err != nil {
				t.Fatalf("runChat: %v", err)
			}
			if got := resp.Choices[0].Message.Content; got != "Hello" {
				t.Errorf("content = %q, want Hello", got)
			}
		})
	}
}

// TestStreamUpstreamFirstOutputIsBounded: the budget bounds the wait for the
// first output (a role chunk does not stop it); without a budget the
// provider's non-streaming ceiling does, as for its unary call.
func TestStreamUpstreamFirstOutputIsBounded(t *testing.T) {
	steps := []sseStep{roleStep(), {hang: true}}

	up := newScriptedUpstream(t, okUnary, steps)
	s := newUnavailTestServer(t, newFakeClock(), providers.NewOpenAICompat("slow", "openai", up.URL, ""))
	_, err := unaryChat(s, soloPlan(routing.TargetOptions{StreamUpstream: boolp(true), TimeoutMS: ms(50)}))
	wantTimeout(t, s, err, errCodeTierTimeout)

	up = newScriptedUpstream(t, okUnary, steps)
	s = newUnavailTestServer(t, newFakeClock(), slowProvider(up.Server))
	_, err = unaryChat(s, soloPlan(streamUpstreamOpts()))
	wantTimeout(t, s, err, providers.ErrCodeUpstreamTimeout)
}

// TestStreamUpstreamSilentUpstreamIsBounded: an upstream that accepts the
// request and never sends even its response headers times out the same
// two ways.
func TestStreamUpstreamSilentUpstreamIsBounded(t *testing.T) {
	up, _ := hangingUpstream(t)
	s := newUnavailTestServer(t, newFakeClock(), providers.NewOpenAICompat("slow", "openai", up.URL, ""))
	_, err := unaryChat(s, soloPlan(routing.TargetOptions{StreamUpstream: boolp(true), TimeoutMS: ms(50)}))
	wantTimeout(t, s, err, errCodeTierTimeout)

	up, _ = hangingUpstream(t)
	s = newUnavailTestServer(t, newFakeClock(), slowProvider(up))
	_, err = unaryChat(s, soloPlan(streamUpstreamOpts()))
	wantTimeout(t, s, err, providers.ErrCodeUpstreamTimeout)
}

// TestStreamUpstreamStallIsATimeout: a stall answers 504 like any timeout,
// leaves the target in rotation the first two times and marks it on the
// third in a row.
func TestStreamUpstreamStallIsATimeout(t *testing.T) {
	up := newScriptedUpstream(t, okUnary, []sseStep{roleStep(), contentStep(0, "Hel"), {hang: true}})
	s := newUnavailTestServer(t, newFakeClock(), providers.NewOpenAICompat("slow", "openai", up.URL, ""))
	plan := soloPlan(routing.TargetOptions{StreamUpstream: boolp(true), StallTimeoutMS: ms(30)})

	for i := 1; i < timeoutsBeforeUnavailable; i++ {
		_, err := unaryChat(s, plan)
		wantTimeout(t, s, err, errCodeUpstreamStall) // also: still unmarked
	}
	if _, err := unaryChat(s, plan); !isTimeout(err) {
		t.Fatalf("third request err = %v, want a stall", err)
	}
	if _, skip := s.unavail.Check(context.Background(), slowKey); !skip {
		t.Fatal("the third stall in a row did not mark the target unavailable")
	}
	if _, err := unaryChat(s, plan); !errors.Is(err, errAllUnavailable) {
		t.Errorf("fourth request err = %v, want %v", err, errAllUnavailable)
	}
	if n := up.streamCalls.Load(); n != 3 {
		t.Errorf("upstream called %d times, want 3", n)
	}
}

// TestStreamUpstreamStallCountsForTheBreaker: a stall is a failure of the
// tier, not a success recorded at its first output.
func TestStreamUpstreamStallCountsForTheBreaker(t *testing.T) {
	up := newScriptedUpstream(t, okUnary, []sseStep{roleStep(), contentStep(0, "Hel"), {hang: true}})
	s := newBreakerTestServer(t, newFakeClock(), providers.NewOpenAICompat("slow", "openai", up.URL, ""), providers.NewMock("mock-ok"))
	opts := routing.TargetOptions{StreamUpstream: boolp(true), StallTimeoutMS: ms(30),
		Breaker: &routing.BreakerOptions{Enabled: boolp(true), Failures: ms(1)}}
	plan := guardedPlan("frog", "slow", opts)

	if res := chat(t, s, plan); res.Provider != "mock-ok" {
		t.Fatalf("served by %q, want the fallback tier", res.Provider)
	}
	if res := chat(t, s, plan); res.Provider != "mock-ok" {
		t.Fatalf("served by %q, want the fallback tier", res.Provider)
	}
	if n := up.streamCalls.Load(); n != 1 {
		t.Errorf("stalling tier called %d times, want 1: one stall must open a breaker set to one failure", n)
	}
}

// TestStreamUpstreamMidStreamFailureFallsThrough: an error event or a
// dropped connection after output started still lets the next tier serve,
// since the client has been sent nothing.
func TestStreamUpstreamMidStreamFailureFallsThrough(t *testing.T) {
	cases := map[string][]sseStep{
		"error event":    {roleStep(), contentStep(0, "Hel"), {data: `{"error":{"message":"boom","code":500}}`}},
		"dropped stream": {roleStep(), contentStep(0, "Hel"), {cut: true}},
	}
	for name, steps := range cases {
		t.Run(name, func(t *testing.T) {
			up := newScriptedUpstream(t, okUnary, steps)
			s := newUnavailTestServer(t, newFakeClock(), providers.NewOpenAICompat("slow", "openai", up.URL, ""), providers.NewMock("mock-ok"))
			plan := budgetPlan(routing.Target{Provider: "slow", UpstreamModel: "m", Options: streamUpstreamOpts()})
			resp, res, err := s.runChat(context.Background(), plan, hiRequest())
			s.bg.Wait()
			if err != nil || res.Provider != "mock-ok" {
				t.Fatalf("served by %q, err %v; want mock-ok", res.Provider, err)
			}
			if strings.Contains(resp.Choices[0].Message.Content, "Hel") && !strings.Contains(resp.Choices[0].Message.Content, "mock-ok") {
				t.Errorf("the failed tier's partial output leaked: %q", resp.Choices[0].Message.Content)
			}
		})
	}
}

// TestStreamUpstreamMissingUsageIsEstimated: a stream that ends without
// usage is billed an estimate — the prompt at rune/4 and one token per chunk
// with output, thinking-only chunks as the reasoning share — never zero.
func TestStreamUpstreamMissingUsageIsEstimated(t *testing.T) {
	up := newScriptedUpstream(t, okUnary, []sseStep{roleStep(), reasoningStep(0, "a"), reasoningStep(0, "b"),
		contentStep(0, "Hel"), contentStep(0, "lo"), finishStep("stop")})
	s := newRunChatTestServer(t, providers.NewOpenAICompat("slow", "openai", up.URL, ""))
	req := llm.ChatRequest{Messages: []llm.Message{{Role: "user", Content: strings.Repeat("word ", 40)}}}

	resp, _, err := s.runChat(context.Background(), soloPlan(streamUpstreamOpts()), req)
	if err != nil {
		t.Fatalf("runChat: %v", err)
	}
	prompt := anthropic.EstimateInputTokens(req)
	want := llm.Usage{PromptTokens: prompt, CompletionTokens: 4, TotalTokens: prompt + 4, ReasoningTokens: 2}
	if resp.Usage != want {
		t.Errorf("usage = %+v, want the estimate %+v", resp.Usage, want)
	}
	if resp.Choices[0].Message.Content != "Hello" {
		t.Errorf("content = %q, want Hello", resp.Choices[0].Message.Content)
	}
}

// TestStreamUpstreamFallsBackToUnary: a request whose answer a stream would
// not carry the same way is served by the unary call.
func TestStreamUpstreamFallsBackToUnary(t *testing.T) {
	for _, extra := range []string{`{"logprobs":true}`, `{"top_logprobs":2}`, `{"n":2}`} {
		up := newScriptedUpstream(t, okUnary, helloSteps())
		s := newRunChatTestServer(t, providers.NewOpenAICompat("slow", "openai", up.URL, ""))
		req := hiRequest()
		if err := json.Unmarshal([]byte(extra), &req.Extra); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.runChat(context.Background(), soloPlan(streamUpstreamOpts()), req); err != nil {
			t.Fatalf("%s: %v", extra, err)
		}
		if up.unaryCalls.Load() != 1 || up.streamCalls.Load() != 0 {
			t.Errorf("%s: %d unary / %d stream calls, want the unary call", extra, up.unaryCalls.Load(), up.streamCalls.Load())
		}
	}
	for _, extra := range []string{`{"logprobs":false}`, `{"n":1}`, `{"top_logprobs":null}`} {
		req := hiRequest()
		_ = json.Unmarshal([]byte(extra), &req.Extra)
		if !streamsUpstream(attemptPolicy{streamUpstream: true}, req) {
			t.Errorf("%s: served unary, want streamed", extra)
		}
	}
}

// TestStreamUpstreamOptionResolution: off by default; the gateway default
// turns it on for a target that does not set it; a target's own value wins.
func TestStreamUpstreamOptionResolution(t *testing.T) {
	cases := []struct {
		name       string
		failover   string
		opt        *bool
		wantStream bool
	}{
		{"default off", ``, nil, false},
		{"gateway default on", `{"stream_upstream":true}`, nil, true},
		{"target off over gateway on", `{"stream_upstream":true}`, boolp(false), false},
		{"target on over gateway off", `{"stream_upstream":false}`, boolp(true), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := newScriptedUpstream(t, okUnary, helloSteps())
			s := newRunChatTestServer(t, providers.NewOpenAICompat("slow", "openai", up.URL, ""))
			if tc.failover != "" {
				s.applyFailover([]byte(tc.failover))
			}
			resp, _, err := s.runChat(context.Background(), soloPlan(routing.TargetOptions{StreamUpstream: tc.opt}), hiRequest())
			if err != nil || resp.Choices[0].Message.Content != "Hello" {
				t.Fatalf("resp %+v err %v", resp, err)
			}
			if got := up.streamCalls.Load() == 1; got != tc.wantStream || up.unaryCalls.Load()+up.streamCalls.Load() != 1 {
				t.Errorf("%d unary / %d stream calls, want streamed=%v", up.unaryCalls.Load(), up.streamCalls.Load(), tc.wantStream)
			}
		})
	}
	s := &Server{}
	s.applyFailover([]byte(`{"stall_timeout_ms":45000}`))
	if pol := s.policyFor(routing.Target{}); pol.stall != 45*time.Second {
		t.Errorf("stall = %v, want the gateway default 45s", pol.stall)
	}
	if pol := s.policyFor(routing.Target{Options: routing.TargetOptions{StallTimeoutMS: ms(2000)}}); pol.stall != 2*time.Second {
		t.Errorf("stall = %v, want the target's 2s", pol.stall)
	}
	if pol := (&Server{}).policyFor(routing.Target{}); pol.stall != defaultStallTimeoutMS*time.Millisecond {
		t.Errorf("stall = %v, want the built-in default", pol.stall)
	}
}

// TestStreamUpstreamLeavesClientStreamsAlone: a client stream on a
// stream_upstream target streams exactly as it would without the option.
func TestStreamUpstreamLeavesClientStreamsAlone(t *testing.T) {
	run := func(opts routing.TargetOptions) []llm.StreamChunk {
		up := newScriptedUpstream(t, okUnary, helloSteps())
		s := newRunChatTestServer(t, providers.NewOpenAICompat("slow", "openai", up.URL, ""))
		var got []llm.StreamChunk
		sink := &recordingSink{onChunk: func(c llm.StreamChunk) { got = append(got, c) }}
		req := hiRequest()
		req.Stream = true
		if _, _, started, err := s.runStream(context.Background(), soloPlan(opts), req, sink); err != nil || !started {
			t.Fatalf("runStream: started=%v err=%v", started, err)
		}
		return got
	}
	plain := run(routing.TargetOptions{StallTimeoutMS: ms(1)})
	with := run(routing.TargetOptions{StreamUpstream: boolp(true), StallTimeoutMS: ms(1)})
	a, _ := json.Marshal(plain)
	b, _ := json.Marshal(with)
	if string(a) != string(b) || len(plain) == 0 {
		t.Errorf("client stream changed:\n with %s\nwithout %s", b, a)
	}
}

// TestStreamUpstreamShowsLiveOutput: while a stream_upstream request is
// assembling, the live view counts its output tokens; it stays a unary
// request in the upstream phase with nothing written to the client.
func TestStreamUpstreamShowsLiveOutput(t *testing.T) {
	up := newScriptedUpstream(t, okUnary, []sseStep{roleStep(), contentStep(0, "a"), reasoningStep(0, "b"), contentStep(0, "c"),
		contentStep(time.Second, "d"), finishStep("stop"), usageStep()})
	s := newRunChatTestServer(t, providers.NewOpenAICompat("slow", "openai", up.URL, ""))
	rec := newLiveRegistry("pod").register(liveRequest{Endpoint: "chat"}, time.Now())
	ctx := withLive(context.Background(), rec)

	done := make(chan error, 1)
	go func() {
		_, _, err := s.runChat(ctx, soloPlan(streamUpstreamOpts()), hiRequest())
		done <- err
	}()
	deadline := time.Now().Add(900 * time.Millisecond)
	var snap liveRequest
	for time.Now().Before(deadline) {
		if snap = rec.snapshot(); snap.OutputTokens == 3 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if snap.OutputTokens != 3 || snap.Stream || snap.Phase != phaseUpstream || snap.BytesOut != 0 {
		t.Errorf("mid-assembly live record = %+v, want 3 tokens, unary, upstream phase, 0 bytes", snap)
	}
	if err := <-done; err != nil {
		t.Fatalf("runChat: %v", err)
	}
	if got := rec.snapshot().OutputTokens; got != 2 {
		t.Errorf("final output tokens = %d, want the upstream's own count 2", got)
	}
}

// TestRunAttemptProgressStopsTheBudgetWithoutCommitting: output that has
// not reached the client stops the budget, and a later failure is that
// failure, not a timeout, with the attempt left uncommitted so the next
// target can serve. Progress after the budget ran out is refused.
func TestRunAttemptProgressStopsTheBudgetWithoutCommitting(t *testing.T) {
	boom := &providers.Error{Status: http.StatusBadGateway, Retryable: true, Message: "boom"}
	committed, err := runAttempt(context.Background(), "slow", 20*time.Millisecond, func(ctx context.Context, g attemptGate) error {
		if !g.progress() {
			t.Error("progress refused within the budget")
		}
		time.Sleep(60 * time.Millisecond)
		if ctx.Err() != nil {
			t.Error("the budget cancelled a call that had made progress")
		}
		return boom
	})
	if committed || err != boom {
		t.Errorf("committed=%v err=%v, want uncommitted and the call's own error", committed, err)
	}

	committed, _ = runAttempt(context.Background(), "slow", 20*time.Millisecond, func(ctx context.Context, g attemptGate) error {
		g.progress()
		if !g.commit() {
			t.Error("commit refused after progress")
		}
		return nil
	})
	if !committed {
		t.Error("a commit after progress did not commit")
	}

	_, err = runAttempt(context.Background(), "slow", 10*time.Millisecond, func(ctx context.Context, g attemptGate) error {
		<-ctx.Done()
		if g.progress() {
			t.Error("progress accepted after the budget ran out")
		}
		return errFirstChunkLate
	})
	if !isTimeout(err) {
		t.Errorf("err = %v, want the budget's timeout", err)
	}
}

// TestObserveChunkCountsThinking: thinking is output in the live count, as
// it is in the upstream's own completion count.
func TestObserveChunkCountsThinking(t *testing.T) {
	rec := newLiveRegistry("pod").register(liveRequest{}, time.Now())
	for _, c := range []llm.StreamChunk{{Role: "assistant"}, {Reasoning: "a"}, {Content: "b"}, {FinishReason: "stop"}} {
		rec.observeChunk(c)
	}
	if got := rec.snapshot().OutputTokens; got != 2 {
		t.Errorf("output tokens = %d, want 2", got)
	}
}
