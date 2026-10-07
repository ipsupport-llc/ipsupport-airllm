package providers

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/llm"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/store"
)

func TestGoogleTTSBaseURL(t *testing.T) {
	cases := []struct {
		cfg      googleCloudConfig
		explicit string
		want     string
	}{
		{googleCloudConfig{Project: "acme"}, "", "https://texttospeech.googleapis.com"},
		{googleCloudConfig{Project: "acme", Location: "eu"}, "", "https://eu-texttospeech.googleapis.com"},
		{googleCloudConfig{Project: "acme", Location: "eu"}, "http://stub:8080/", "http://stub:8080"},
	}
	for _, c := range cases {
		if got := googleTTSBaseURL(c.cfg, c.explicit); got != c.want {
			t.Errorf("googleTTSBaseURL(%+v, %q) = %q, want %q", c.cfg, c.explicit, got, c.want)
		}
	}
}

// TestGoogleTTSConfigNeedsAProject: the project is the quota project
// billed, so an address cannot stand in for it.
func TestGoogleTTSConfigNeedsAProject(t *testing.T) {
	if err := ValidateProviderConfig(KindGoogleTTS, []byte(`{}`), "http://stub"); err == nil {
		t.Error("a google-tts provider without a project was accepted")
	}
	if err := ValidateProviderConfig(KindGoogleTTS, []byte(`{"project":"acme"}`), ""); err != nil {
		t.Errorf("a google-tts provider with a project was refused: %v", err)
	}
}

// TestGoogleTTSFromRowAuthenticatesWithTheStoredCredential builds the
// provider the way the registry does and checks the token it mints; a
// credential that does not resolve disables it instead.
func TestGoogleTTSFromRowAuthenticatesWithTheStoredCredential(t *testing.T) {
	var hits atomic.Int32
	tokens := tokenEndpoint(t, &hits)
	row := store.ProviderRow{Name: "gtts", Kind: KindGoogleTTS, Config: json.RawMessage(`{"project":"acme","location":"eu"}`)}
	g, err := newGoogleTTSFromRow(context.Background(), row, serviceAccountJSON(t, "google-tts-row", tokens.URL), nil)
	if err != nil {
		t.Fatalf("newGoogleTTSFromRow: %v", err)
	}
	if g.baseURL != "https://eu-texttospeech.googleapis.com" || g.project != "acme" {
		t.Errorf("built %s for project %q, want the eu host for acme", g.baseURL, g.project)
	}
	if tok, err := g.tokens.Token(context.Background()); err != nil || tok != "ya29.stub" {
		t.Errorf("token = %q, %v; want the stored credential's", tok, err)
	}
	if _, err := newGoogleTTSFromRow(context.Background(), row, []byte(`{"type":"nonsense"}`), nil); err == nil {
		t.Error("credential bytes that do not resolve were accepted; want the provider disabled")
	}
}

func TestGoogleTTSDeclaresSynthesisOnly(t *testing.T) {
	var p Provider = NewGoogleTTS("gtts", "http://stub", "acme", nil)
	if _, ok := p.(Synthesizer); !ok {
		t.Error("google-tts does not synthesize")
	}
	if _, ok := p.(VoiceLister); !ok {
		t.Error("google-tts does not list its voices")
	}
	if _, ok := p.(Transcriber); ok {
		t.Error("google-tts claims to transcribe; an audio alias could resolve to it and fail at request time")
	}
	if _, err := p.Chat(context.Background(), llm.ChatRequest{Model: "neural2"}); err == nil || IsFallbackWorthy(err) {
		t.Errorf("chat on google-tts: %v, want a non-fallback configuration error", err)
	}
}

func TestGoogleVoiceFamily(t *testing.T) {
	for voice, want := range map[string]string{
		"en-US-Chirp3-HD-Charon": "chirp3-hd",
		"en-US-Neural2-F":        "neural2",
		"cmn-CN-Wavenet-A":       "wavenet",
		"en-US-Standard-B":       "standard",
		"en_US-lessac":           "",
		"alloy":                  "",
		"en-US-Casual":           "",
	} {
		if got := googleVoiceFamily(voice); got != want {
			t.Errorf("googleVoiceFamily(%q) = %q, want %q", voice, got, want)
		}
	}
}
