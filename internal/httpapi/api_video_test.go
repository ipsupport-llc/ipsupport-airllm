package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/audio"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/routing"
)

// videoUpstream answers /video/speech with head, waits for release, then sends
// tail; status other than 200 answers that status instead. got receives each body.
func videoUpstream(t *testing.T, status int, head, tail []byte, release <-chan struct{}, got chan<- map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/video/speech" {
			t.Errorf("upstream path %q, want /video/speech", r.URL.Path)
		}
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		if got != nil {
			got <- b
		}
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write(head)
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = w.Write(tail)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The video reaches the client as the upstream makes it, with the language
// sent upstream.
func TestVideoSpeechStreamsAsItArrives(t *testing.T) {
	head, tail := bytes.Repeat([]byte("ftyp"), 64), bytes.Repeat([]byte("moof"), 512)
	release := make(chan struct{})
	got := make(chan map[string]any, 1)
	up := videoUpstream(t, http.StatusOK, head, tail, release, got)
	s := streamTestServer(t, map[string]string{"local": up.URL})
	w := newRecordingWriter()
	type result struct {
		streamed bool
		err      error
	}
	done := make(chan result, 1)
	go func() {
		_, streamed, err := s.runVideoTo(context.Background(), ttsPlan(routing.Target{Provider: "local", UpstreamModel: "chatterbox"}),
			audio.SpeechRequest{Input: "Привет", Language: "ru", ResponseFormat: "mp4", Placeholder: new(bool)}, &speechSink{w: w, flush: w.Flush})
		done <- result{streamed, err}
	}()
	select {
	case n := <-w.flushed:
		if n != len(head) {
			t.Fatalf("first flush carried %d bytes, want %d", n, len(head))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no video reached the client while the upstream was still rendering")
	}
	close(release)
	r := <-done
	if r.err != nil || !r.streamed {
		t.Fatalf("streamed %v err %v", r.streamed, r.err)
	}
	if !bytes.Equal(w.bytes(), append(head, tail...)) || w.header.Get("Content-Type") != "video/mp4" {
		t.Fatalf("client got %d bytes, type %q", len(w.bytes()), w.header.Get("Content-Type"))
	}
	if b := <-got; b["model"] != "chatterbox" || b["input"] != "Привет" || b["language"] != "ru" || b["placeholder"] != false {
		t.Fatalf("upstream got %v", b)
	}
}

// A tier that fails before any video went out passes the request on.
func TestVideoSpeechFailsOverBeforeTheFirstByte(t *testing.T) {
	release := make(chan struct{})
	close(release)
	bad := videoUpstream(t, http.StatusServiceUnavailable, nil, nil, release, nil)
	good := videoUpstream(t, http.StatusOK, bytes.Repeat([]byte("v"), 100), nil, release, nil)
	s := streamTestServer(t, map[string]string{"bad": bad.URL, "good": good.URL})
	w := newRecordingWriter()
	res, streamed, err := s.runVideoTo(context.Background(),
		ttsPlan(routing.Target{Provider: "bad", UpstreamModel: "m"}, routing.Target{Provider: "good", UpstreamModel: "m"}),
		audio.SpeechRequest{Input: "hi", ResponseFormat: "mp4"}, &speechSink{w: w, flush: w.Flush})
	if err != nil || !streamed || res.Provider != "good" || len(w.bytes()) != 100 {
		t.Fatalf("provider %q streamed %v err %v, %d bytes", res.Provider, streamed, err, len(w.bytes()))
	}
}

// When every tier fails, nothing was sent and the caller writes the error.
func TestVideoSpeechAllTiersFail(t *testing.T) {
	release := make(chan struct{})
	close(release)
	bad := videoUpstream(t, http.StatusServiceUnavailable, nil, nil, release, nil)
	s := streamTestServer(t, map[string]string{"bad": bad.URL})
	w := newRecordingWriter()
	_, streamed, err := s.runVideoTo(context.Background(), ttsPlan(routing.Target{Provider: "bad", UpstreamModel: "m"}),
		audio.SpeechRequest{Input: "hi", ResponseFormat: "mp4"}, &speechSink{w: w, flush: w.Flush})
	if err == nil || streamed || len(w.bytes()) != 0 {
		t.Fatalf("streamed %v err %v, %d bytes; want an error and nothing sent", streamed, err, len(w.bytes()))
	}
}
