package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/llm"
)

// stubTokenSource stands in for a real OAuth2 exchange, so every test here
// runs offline. A non-empty err makes the exchange fail.
type stubTokenSource struct {
	token string
	err   error
}

func (s stubTokenSource) Token(context.Context) (string, error) {
	if s.err != nil {
		return "", s.err
	}
	return s.token, nil
}

func TestVertexBaseURL(t *testing.T) {
	cfg := vertexConfig{Project: "acme", Location: "us-west1"}
	cases := []struct {
		name     string
		cfg      vertexConfig
		explicit string
		want     string
	}{
		{
			"regional location prefixes the host",
			cfg, "",
			"https://us-west1-aiplatform.googleapis.com/v1/projects/acme/locations/us-west1/endpoints/openapi",
		},
		{
			"the global location uses the unprefixed host",
			vertexConfig{Project: "acme", Location: "global"}, "",
			"https://aiplatform.googleapis.com/v1/projects/acme/locations/global/endpoints/openapi",
		},
		{
			"an unset location is global",
			vertexConfig{Project: "acme"}, "",
			"https://aiplatform.googleapis.com/v1/projects/acme/locations/global/endpoints/openapi",
		},
		{
			"an explicit address wins over both, verbatim",
			cfg, "http://127.0.0.1:8080/v1beta1/openapi",
			"http://127.0.0.1:8080/v1beta1/openapi",
		},
		{
			"an explicit address loses only its trailing slash",
			cfg, "http://127.0.0.1:8080/stub/",
			"http://127.0.0.1:8080/stub",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := vertexBaseURL(c.cfg, c.explicit); got != c.want {
				t.Errorf("vertexBaseURL() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestParseVertexConfig(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    vertexConfig
		wantErr bool
	}{
		{"absent config", "", vertexConfig{}, false},
		{"the column default", "{}", vertexConfig{}, false},
		{"both values", `{"project":"acme","location":"us-west1"}`, vertexConfig{Project: "acme", Location: "us-west1"}, false},
		{"surrounding whitespace is trimmed", `{"project":"  acme ","location":" global "}`, vertexConfig{Project: "acme", Location: "global"}, false},
		{"unrelated keys are ignored", `{"project":"acme","nonsense":1}`, vertexConfig{Project: "acme"}, false},
		{"malformed json", `{"project":`, vertexConfig{}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseVertexConfig([]byte(c.raw))
			if c.wantErr {
				if err == nil {
					t.Fatalf("want an error, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseVertexConfig: %v", err)
			}
			if got != c.want {
				t.Errorf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestValidateVertexConfig(t *testing.T) {
	cases := []struct {
		name    string
		cfg     vertexConfig
		baseURL string
		wantErr bool
	}{
		{"a project is enough", vertexConfig{Project: "acme"}, "", false},
		{"an explicit address is enough on its own", vertexConfig{}, "http://127.0.0.1:8080", false},
		{"neither is rejected", vertexConfig{}, "", true},
		{"a location alone is not a configuration", vertexConfig{Location: "us-west1"}, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateVertexConfig(c.cfg, c.baseURL)
			if (err != nil) != c.wantErr {
				t.Errorf("validateVertexConfig() error = %v, wantErr %v", err, c.wantErr)
			}
		})
	}
}

func TestNormalizeVertexModel(t *testing.T) {
	cases := []struct{ in, want string }{
		{"gemini-2.5-flash", "google/gemini-2.5-flash"},
		{"google/gemini-2.5-flash", "google/gemini-2.5-flash"},
		{"meta/llama-4", "meta/llama-4"},
		{"", ""},
	}
	for _, c := range cases {
		if got := normalizeVertexModel(c.in); got != c.want {
			t.Errorf("normalizeVertexModel(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestVertexDeclaresNoAudioCapability pins down the capability surface. The
// point is not that Vertex lacks audio methods today — it is that a later
// refactor reaching for OpenAICompat by embedding it would grow them
// silently, and an audio alias would then resolve here and fail at request
// time instead of being impossible to configure.
func TestVertexDeclaresNoAudioCapability(t *testing.T) {
	var p any = NewVertex("vx", "http://127.0.0.1", stubTokenSource{token: "t"})

	if _, ok := p.(Transcriber); ok {
		t.Error("Vertex must not satisfy Transcriber: it would let an audio alias resolve to a provider that cannot serve it")
	}
	if _, ok := p.(Synthesizer); ok {
		t.Error("Vertex must not satisfy Synthesizer: it would let an audio alias resolve to a provider that cannot serve it")
	}
	if _, ok := p.(PricedModelLister); ok {
		t.Error("Vertex must not satisfy PricedModelLister: Google publishes no machine-readable price list, so an import would silently do nothing")
	}
	if _, ok := p.(ModelLister); !ok {
		t.Error("Vertex must satisfy ModelLister: the alias editor's dropdown is its only source of Gemini model ids")
	}
}

func TestVertexListModelsIsCuratedAndNotAliased(t *testing.T) {
	p := NewVertex("vx", "http://127.0.0.1", stubTokenSource{token: "t"})
	first, err := p.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(first) == 0 {
		t.Fatal("the curated list must not be empty; it is the alias editor's whole dropdown")
	}
	for _, m := range first {
		if !strings.HasPrefix(m, vertexDefaultPublisher+"/") {
			t.Errorf("curated model %q is missing its publisher prefix; alias targets copied from here would be rejected upstream", m)
		}
	}

	first[0] = "clobbered"
	second, err := p.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if second[0] == "clobbered" {
		t.Error("ListModels handed out the package-level slice; one caller mutating it would corrupt the list for every other")
	}
}

// vertexUpstream is an in-process stand-in for Vertex's OpenAI-compatible
// surface. It records the one request it is given and replies with resp.
type vertexUpstream struct {
	*httptest.Server
	path string
	auth string
	body map[string]any
}

func newVertexUpstream(t *testing.T, status int, contentType, resp string) *vertexUpstream {
	t.Helper()
	up := &vertexUpstream{}
	up.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		up.path = r.URL.Path
		up.auth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&up.body)
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(status)
		fmt.Fprint(w, resp)
	}))
	t.Cleanup(up.Close)
	return up
}

func TestVertexChatQualifiesTheModelAndBearsTheToken(t *testing.T) {
	up := newVertexUpstream(t, http.StatusOK, "application/json",
		`{"id":"c1","model":"google/gemini-2.5-flash","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`)

	p := NewVertex("vx", up.URL, stubTokenSource{token: "ya29.stub"})
	resp, err := p.Chat(context.Background(), llm.ChatRequest{
		Model:    "gemini-2.5-flash", // deliberately unqualified
		Messages: []llm.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	if up.path != "/chat/completions" {
		t.Errorf("upstream path = %q, want /chat/completions appended to the configured address", up.path)
	}
	if up.auth != "Bearer ya29.stub" {
		t.Errorf("Authorization = %q, want the token source's token as a bearer", up.auth)
	}
	if got := up.body["model"]; got != "google/gemini-2.5-flash" {
		t.Errorf("upstream model = %v, want the publisher-qualified id", got)
	}
	if resp.Usage.TotalTokens != 10 {
		t.Errorf("usage lost: %+v", resp.Usage)
	}
}

func TestVertexTokenSourceFailureIsFallbackWorthy(t *testing.T) {
	// No upstream at all: the call must fail before it is ever made.
	p := NewVertex("vx", "http://127.0.0.1:1", stubTokenSource{err: errors.New("metadata server unreachable")})

	_, err := p.Chat(context.Background(), llm.ChatRequest{Model: "gemini-2.5-flash"})
	if err == nil {
		t.Fatal("want an error when the token source fails")
	}
	if !IsFallbackWorthy(err) {
		t.Errorf("a token-source failure must be fallback-worthy, not fatal, so another tier can serve the request; got %v", err)
	}

	streamErr := p.ChatStream(context.Background(), llm.ChatRequest{Model: "gemini-2.5-flash"},
		func(llm.StreamChunk) error { return nil })
	if !IsFallbackWorthy(streamErr) {
		t.Errorf("the streaming path must classify a token-source failure the same way; got %v", streamErr)
	}
}

func TestVertexChatStreamReportsUsageOnce(t *testing.T) {
	// Vertex reports cumulative usage on chunk after chunk, which is exactly
	// what the coalescing decoder exists for.
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, line := range []string{
			`{"choices":[{"delta":{"role":"assistant"}}],"usage":{"prompt_tokens":7,"completion_tokens":0,"total_tokens":7}}`,
			`{"choices":[{"delta":{"content":"he"}}],"usage":{"prompt_tokens":7,"completion_tokens":1,"total_tokens":8}}`,
			`{"choices":[{"delta":{"content":"llo"}}],"usage":{"prompt_tokens":7,"completion_tokens":2,"total_tokens":9}}`,
			`{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":2,"total_tokens":9}}`,
			"[DONE]",
		} {
			fmt.Fprintf(w, "data: %s\n\n", line)
		}
	}))
	defer up.Close()

	p := NewVertex("vx", up.URL, stubTokenSource{token: "t"})
	var got []llm.StreamChunk
	if err := p.ChatStream(context.Background(), llm.ChatRequest{Model: "gemini-2.5-flash"},
		func(c llm.StreamChunk) error { got = append(got, c); return nil }); err != nil {
		t.Fatalf("ChatStream: %v", err)
	}

	var usageChunks []llm.Usage
	for _, c := range got {
		if c.Usage != nil {
			usageChunks = append(usageChunks, *c.Usage)
		}
	}
	if len(usageChunks) != 1 {
		t.Fatalf("want exactly 1 usage chunk from 4 cumulative reports, got %d", len(usageChunks))
	}
	if usageChunks[0].TotalTokens != 9 {
		t.Errorf("coalesced usage = %+v, want the final totals", usageChunks[0])
	}
	if last := got[len(got)-1]; last.Usage == nil {
		t.Errorf("usage must be the last chunk, after the finish signal; got %+v", last)
	}
}
