package providers

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/llm"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/store"
)

func TestGoogleSpeechBaseURL(t *testing.T) {
	cases := []struct {
		cfg      googleCloudConfig
		explicit string
		want     string
	}{
		{googleCloudConfig{Project: "acme"}, "", "https://speech.googleapis.com"},
		{googleCloudConfig{Project: "acme", Location: "global"}, "", "https://speech.googleapis.com"},
		{googleCloudConfig{Project: "acme", Location: "europe-west4"}, "", "https://europe-west4-speech.googleapis.com"},
		{googleCloudConfig{Project: "acme", Location: "europe-west4"}, "http://stub:8080/", "http://stub:8080"},
	}
	for _, c := range cases {
		if got := googleSpeechBaseURL(c.cfg, c.explicit); got != c.want {
			t.Errorf("googleSpeechBaseURL(%+v, %q) = %q, want %q", c.cfg, c.explicit, got, c.want)
		}
	}
}

// TestGoogleSpeechConfigNeedsAProject: the project is in every request's
// path and is the quota project billed, so an address cannot stand in for it
// the way it can for Vertex.
func TestGoogleSpeechConfigNeedsAProject(t *testing.T) {
	if err := ValidateProviderConfig(KindGoogleSpeech, []byte(`{}`), "http://stub"); err == nil {
		t.Error("a google-speech provider without a project was accepted")
	}
	if err := ValidateProviderConfig(KindGoogleSpeech, []byte(`{"project":"acme"}`), ""); err != nil {
		t.Errorf("a google-speech provider with a project was refused: %v", err)
	}
}

// TestNewGoogleSpeechFromRowDisablesRatherThanGuesses mirrors the Vertex
// test: every refusal ends with the provider absent, never with the gateway
// quietly running as the pod's own identity. The last case is the one that
// reaches credential resolution: bytes that do not resolve disable the
// provider rather than fall back.
func TestNewGoogleSpeechFromRowDisablesRatherThanGuesses(t *testing.T) {
	good := json.RawMessage(`{"project":"acme"}`)
	cases := []struct {
		name    string
		config  json.RawMessage
		cred    []byte
		credErr error
	}{
		{"a credential that cannot be decrypted", good, nil, errors.New("cipher: message authentication failed")},
		{"a malformed configuration", json.RawMessage(`{"project":`), nil, nil},
		{"no project", json.RawMessage(`{}`), nil, nil},
		{"credential bytes that do not resolve", good, []byte(`{"type":"nonsense"}`), nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			row := store.ProviderRow{Name: "gstt", Kind: KindGoogleSpeech, Config: c.config}
			if got, err := newGoogleSpeechFromRow(context.Background(), row, c.cred, c.credErr); err == nil {
				t.Fatalf("want the provider disabled with an error, got %+v", got)
			}
		})
	}
}

// TestGoogleSpeechFromRowAuthenticatesWithTheStoredCredential builds the
// provider the way the registry does, from a service-account credential
// whose token endpoint is local, and checks the token it mints.
func TestGoogleSpeechFromRowAuthenticatesWithTheStoredCredential(t *testing.T) {
	var hits atomic.Int32
	tokens := tokenEndpoint(t, &hits)
	row := store.ProviderRow{Name: "gstt", Kind: KindGoogleSpeech, Config: json.RawMessage(`{"project":"acme","location":"eu"}`)}
	g, err := newGoogleSpeechFromRow(context.Background(), row, serviceAccountJSON(t, "google-speech-row", tokens.URL), nil)
	if err != nil {
		t.Fatalf("newGoogleSpeechFromRow: %v", err)
	}
	if g.baseURL != "https://eu-speech.googleapis.com" || g.project != "acme" || g.location != "eu" {
		t.Errorf("built %s for project %q location %q, want the eu host for acme", g.baseURL, g.project, g.location)
	}
	if tok, err := g.tokens.Token(context.Background()); err != nil || tok != "ya29.stub" {
		t.Errorf("token = %q, %v; want the stored credential's", tok, err)
	}
}

func TestGoogleSpeechDeclaresTranscriptionOnly(t *testing.T) {
	var p Provider = NewGoogleSpeech("gstt", "http://stub", "acme", "", nil)
	if _, ok := p.(Transcriber); !ok {
		t.Error("google-speech does not transcribe")
	}
	if _, ok := p.(Synthesizer); ok {
		t.Error("google-speech claims to synthesize; an audio alias could resolve to it and fail at request time")
	}
	if _, err := p.Chat(context.Background(), llm.ChatRequest{Model: "long"}); err == nil || IsFallbackWorthy(err) {
		t.Errorf("chat on google-speech: %v, want a non-fallback configuration error", err)
	}
}
