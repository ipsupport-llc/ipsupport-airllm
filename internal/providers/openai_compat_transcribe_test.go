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

// whisper.cpp's server joins its segments' texts with newlines in the
// verbose text, and a segment can end mid-word. The segments carry their own
// leading spaces, so the transcript is their plain concatenation.
func TestOpenAICompatTranscribeJoinsSegmentsAsSpoken(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"text":" Я хотел бы узнать, чем занимается ваша комп\nания.\n","language":"russian","duration":4.6,
			"segments":[{"text":" Я хотел бы узнать, чем занимается ваша комп","avg_logprob":-0.1},{"text":"ания.","avg_logprob":-0.1}]}`))
	}))
	defer ts.Close()

	p := NewOpenAICompat("up", "openai", ts.URL, "")
	resp, err := p.Transcribe(context.Background(), audio.TranscriptionRequest{Model: "whisper", Audio: []byte("x"), Filename: "a.wav"})
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if want := "Я хотел бы узнать, чем занимается ваша компания."; resp.Text != want {
		t.Errorf("Text = %q, want %q", resp.Text, want)
	}
}

// fakeWhisper answers each transcription with the language the request
// forced, or with detected when it forced none, and records the language
// field of every request it saw ("" for none).
func fakeWhisper(t *testing.T, detected string, forcedLanguages *[]string) *httptest.Server {
	t.Helper()
	names := map[string]string{"en": "english", "es": "spanish", "pt": "portuguese"}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatalf("server: ParseMultipartForm: %v", err)
		}
		lang, sent := "", r.MultipartForm.Value["language"]
		if len(sent) > 0 {
			lang = sent[0]
		}
		*forcedLanguages = append(*forcedLanguages, lang)
		name := detected
		if lang != "" {
			name = names[lang]
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"text":"heard as ` + name + `","language":"` + name + `","duration":2.0}`))
	}))
}

// With alternatives the Whisper family must not be told the language: it
// treats language as an order and translates speech in an alternative into
// the primary. It detects instead, and a detection among the request's
// languages is the answer.
func TestOpenAICompatTranscribeWithAlternativesDetects(t *testing.T) {
	var forced []string
	ts := fakeWhisper(t, "spanish", &forced)
	defer ts.Close()

	p := NewOpenAICompat("up", "openai", ts.URL, "")
	resp, err := p.Transcribe(context.Background(), audio.TranscriptionRequest{
		Model: "whisper", Audio: []byte("x"), Filename: "a.wav",
		Language: "en-US", AlternativeLanguages: []string{"es-ES"},
	})
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if len(forced) != 1 || forced[0] != "" {
		t.Fatalf("upstream language fields = %q, want one request with none", forced)
	}
	if resp.Text != "heard as spanish" || resp.Language != "es" {
		t.Errorf("got %q in %q, want the detected Spanish transcript", resp.Text, resp.Language)
	}
}

// A detection outside the request's languages — a short utterance heard as
// Portuguese — must not leak a third language into the call: the audio is
// decoded once more with the primary forced, and that is the answer.
func TestOpenAICompatTranscribeStrayDetectionForcesPrimary(t *testing.T) {
	var forced []string
	ts := fakeWhisper(t, "portuguese", &forced)
	defer ts.Close()

	p := NewOpenAICompat("up", "openai", ts.URL, "")
	resp, err := p.Transcribe(context.Background(), audio.TranscriptionRequest{
		Model: "whisper", Audio: []byte("x"), Filename: "a.wav",
		Language: "en-US", AlternativeLanguages: []string{"es-ES"},
	})
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if len(forced) != 2 || forced[0] != "" || forced[1] != "en" {
		t.Fatalf("upstream language fields = %q, want detection then en forced", forced)
	}
	if resp.Text != "heard as english" || resp.Language != "en" {
		t.Errorf("got %q in %q, want the forced English transcript", resp.Text, resp.Language)
	}
}

// Without alternatives nothing changes: the primary language is forced in
// the bare form Whisper accepts, and no language means detection.
func TestOpenAICompatTranscribeWithoutAlternatives(t *testing.T) {
	for _, tc := range []struct{ language, want string }{{"en-US", "en"}, {"", ""}} {
		var forced []string
		ts := fakeWhisper(t, "spanish", &forced)
		p := NewOpenAICompat("up", "openai", ts.URL, "")
		if _, err := p.Transcribe(context.Background(), audio.TranscriptionRequest{
			Model: "whisper", Audio: []byte("x"), Filename: "a.wav", Language: tc.language,
		}); err != nil {
			t.Fatalf("Transcribe(%q): %v", tc.language, err)
		}
		ts.Close()
		if len(forced) != 1 || forced[0] != tc.want {
			t.Errorf("language %q: upstream language fields = %q, want one request with %q", tc.language, forced, tc.want)
		}
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
