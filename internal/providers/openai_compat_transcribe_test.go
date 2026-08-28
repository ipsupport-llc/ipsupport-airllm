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
