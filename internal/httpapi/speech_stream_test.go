package httpapi

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/audio"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/metrics"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/providers"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/routing"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/speechcache"
)

// recordingWriter is a ResponseWriter that reports every flushed write.
type recordingWriter struct {
	mu      sync.Mutex
	header  http.Header
	status  int
	body    bytes.Buffer
	flushed chan int // the body size at each flush
}

func newRecordingWriter() *recordingWriter {
	return &recordingWriter{header: http.Header{}, flushed: make(chan int, 64)}
}

func (w *recordingWriter) Header() http.Header { return w.header }
func (w *recordingWriter) WriteHeader(code int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.status = code
}
func (w *recordingWriter) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(b)
}
func (w *recordingWriter) Flush() {
	w.mu.Lock()
	n := w.body.Len()
	w.mu.Unlock()
	select {
	case w.flushed <- n:
	default:
	}
}
func (w *recordingWriter) bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return bytes.Clone(w.body.Bytes())
}

// streamingUpstream answers /audio/speech with head, waits for release (or
// fails when cut is set), then sends tail.
func streamingUpstream(t *testing.T, contentType string, head, tail []byte, release <-chan struct{}, cut bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		_, _ = w.Write(head)
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		if cut {
			// Abort the connection mid-body.
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Error("cannot hijack")
				return
			}
			c, _, _ := hj.Hijack()
			_ = c.Close()
			return
		}
		_, _ = w.Write(tail)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func streamTestServer(t *testing.T, ups map[string]string) *Server {
	t.Helper()
	reg := providers.NewRegistry()
	for name, url := range ups {
		reg.Register(providers.NewOpenAICompat(name, "openai", url, ""), 4)
	}
	s := &Server{router: routing.NewRouter(nil), metrics: metrics.New(), speechCache: speechcache.New(nil)}
	s.regPtr.Store(reg)
	return s
}

func streamingTarget(provider string, stream bool) routing.Target {
	t := routing.Target{Provider: provider, UpstreamModel: "chatterbox"}
	if stream {
		t.Options.StreamSpeech = &stream
	}
	return t
}

// A stream_speech target's audio reaches the client while the upstream is
// still synthesizing.
func TestStreamSpeechPassesAudioOnBeforeTheUpstreamFinishes(t *testing.T) {
	wav := pcmWAV(24000, 1)
	head, tail := wav[:4096], wav[4096:]
	release := make(chan struct{})
	up := streamingUpstream(t, "audio/wav", head, tail, release, false)
	s := streamTestServer(t, map[string]string{"local": up.URL})
	w := newRecordingWriter()
	sink := &speechSink{w: w, flush: w.Flush}

	type result struct {
		streamed bool
		err      error
	}
	done := make(chan result, 1)
	go func() {
		_, _, streamed, err := s.runSynthesizeTo(context.Background(), ttsPlan(streamingTarget("local", true)),
			audio.SpeechRequest{Input: "hi", ResponseFormat: "wav"}, sink)
		done <- result{streamed, err}
	}()
	select {
	case n := <-w.flushed:
		if n != len(head) {
			t.Fatalf("first flush carried %d bytes, want the %d the upstream sent", n, len(head))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no audio reached the client while the upstream was still synthesizing")
	}
	close(release)
	r := <-done
	if r.err != nil || !r.streamed {
		t.Fatalf("streamed %v err %v, want a clean stream", r.streamed, r.err)
	}
	if !bytes.Equal(w.bytes(), wav) || w.header.Get("Content-Type") != "audio/wav" || w.status != http.StatusOK {
		t.Fatalf("client got %d bytes, type %q, status %d; want the whole %d-byte WAV", len(w.bytes()), w.header.Get("Content-Type"), w.status, len(wav))
	}
}

// Without the option the reply is the whole clip, written by the handler as before.
func TestSpeechWithoutStreamSpeechIsAWholeClip(t *testing.T) {
	wav := pcmWAV(24000, 1)
	up, _ := fakeOpenAISpeech(t, "audio/wav", wav)
	s := streamTestServer(t, map[string]string{"local": up.URL})
	w := newRecordingWriter()
	resp, _, streamed, err := s.runSynthesizeTo(context.Background(), ttsPlan(streamingTarget("local", false)),
		audio.SpeechRequest{Input: "hi", ResponseFormat: "wav"}, &speechSink{w: w, flush: w.Flush})
	if err != nil || streamed || len(w.bytes()) != 0 || !bytes.Equal(resp.Audio, wav) {
		t.Fatalf("streamed %v err %v, %d bytes written; want the whole clip returned, nothing written", streamed, err, len(w.bytes()))
	}
}

// A stream that is not the WAV asked for is caught before any byte is sent,
// so the next tier still answers.
func TestStreamSpeechFallsOverOnABadWAVBeforeSendingAnything(t *testing.T) {
	release := make(chan struct{})
	close(release)
	bad := streamingUpstream(t, "audio/mpeg", bytes.Repeat([]byte{0xff}, 200), nil, release, false)
	wav := pcmWAV(16000, 1)
	good, _ := fakeOpenAISpeech(t, "audio/wav", wav)
	s := streamTestServer(t, map[string]string{"bad": bad.URL, "good": good.URL})
	w := newRecordingWriter()
	resp, res, streamed, err := s.runSynthesizeTo(context.Background(),
		ttsPlan(streamingTarget("bad", true), streamingTarget("good", false)),
		audio.SpeechRequest{Input: "hi", ResponseFormat: "wav"}, &speechSink{w: w, flush: w.Flush})
	if err != nil || streamed || res.Provider != "good" || len(w.bytes()) != 0 || !bytes.Equal(resp.Audio, wav) {
		t.Fatalf("provider %q streamed %v err %v, %d bytes written; want the good tier's whole clip", res.Provider, streamed, err, len(w.bytes()))
	}
}

// An upstream that breaks off after audio went out ends the reply: no other
// tier takes over, and the caller learns it had started.
func TestStreamSpeechBrokenMidStreamReportsStarted(t *testing.T) {
	wav := pcmWAV(24000, 1)
	release := make(chan struct{})
	close(release)
	up := streamingUpstream(t, "audio/wav", wav[:4096], nil, release, true)
	other, calls := fakeOpenAISpeech(t, "audio/wav", wav)
	s := streamTestServer(t, map[string]string{"local": up.URL, "other": other.URL})
	w := newRecordingWriter()
	_, res, streamed, err := s.runSynthesizeTo(context.Background(),
		ttsPlan(streamingTarget("local", true), streamingTarget("other", false)),
		audio.SpeechRequest{Input: "hi", ResponseFormat: "wav"}, &speechSink{w: w, flush: w.Flush})
	if err == nil || !streamed || res.Provider != "local" {
		t.Fatalf("provider %q streamed %v err %v; want the broken stream reported as started", res.Provider, streamed, err)
	}
	if res.Cache != "" {
		t.Errorf("cache outcome %q on an alias without the cache, want none", res.Cache)
	}
	if len(calls()) != 0 {
		t.Fatal("another tier was called after audio had been sent")
	}
	if !bytes.Equal(w.bytes(), wav[:4096]) {
		t.Fatalf("client got %d bytes, want the 4096 sent before the break", len(w.bytes()))
	}
}

// A streamed miss is stored, so the repeat is answered from the cache whole.
func TestStreamSpeechMissIsCachedForTheRepeat(t *testing.T) {
	wav := pcmWAV(24000, 1)
	release := make(chan struct{})
	close(release)
	up := streamingUpstream(t, "audio/wav", wav[:4096], wav[4096:], release, false)
	s := streamTestServer(t, map[string]string{"local": up.URL})
	plan := ttsPlan(streamingTarget("local", true))
	plan.SynthesisCache, plan.SynthesisCacheTTL = true, routing.DefaultSynthesisCacheTTL

	w := newRecordingWriter()
	_, res, streamed, err := s.runSynthesizeTo(context.Background(), plan, audio.SpeechRequest{Input: "hi", ResponseFormat: "wav"}, &speechSink{w: w, flush: w.Flush})
	if err != nil || !streamed || res.Cache != cacheMiss {
		t.Fatalf("first: streamed %v cache %q err %v", streamed, res.Cache, err)
	}
	up.Close() // the repeat must not need the upstream
	w2 := newRecordingWriter()
	resp, res, streamed, err := s.runSynthesizeTo(context.Background(), plan, audio.SpeechRequest{Input: "hi", ResponseFormat: "wav"}, &speechSink{w: w2, flush: w2.Flush})
	if err != nil || streamed || res.Cache != cacheHit || !bytes.Equal(resp.Audio, wav) {
		t.Fatalf("repeat: streamed %v cache %q err %v, %d bytes; want the whole clip from the cache", streamed, res.Cache, err, len(resp.Audio))
	}
}

// A WAV whose header runs past the upstream's first flush (a LIST chunk
// before the data) is read on to the data chunk, not rejected.
func TestStreamSpeechReadsALongWAVHeaderAcrossChunks(t *testing.T) {
	wav := pcmWAV(24000, 1)
	list := append([]byte("LIST"), 0, 0, 0, 0)
	meta := bytes.Repeat([]byte("x"), 200)
	list[4] = byte(len(meta))
	long := append(append(append(append([]byte(nil), wav[:36]...), list...), meta...), wav[36:]...)
	release := make(chan struct{})
	close(release)
	up := streamingUpstream(t, "audio/wav", long[:100], long[100:], release, false)
	s := streamTestServer(t, map[string]string{"local": up.URL})
	w := newRecordingWriter()
	_, _, streamed, err := s.runSynthesizeTo(context.Background(), ttsPlan(streamingTarget("local", true)),
		audio.SpeechRequest{Input: "hi", ResponseFormat: "wav"}, &speechSink{w: w, flush: w.Flush})
	if err != nil || !streamed || !bytes.Equal(w.bytes(), long) {
		t.Fatalf("streamed %v err %v, %d of %d bytes; want the whole WAV with its LIST chunk", streamed, err, len(w.bytes()), len(long))
	}
}

// A cached alias whose stream breaks reports a miss (a provider rendered it)
// and stores nothing.
func TestStreamSpeechBrokenOnACachedAliasIsAMissNotStored(t *testing.T) {
	wav := pcmWAV(24000, 1)
	release := make(chan struct{})
	close(release)
	up := streamingUpstream(t, "audio/wav", wav[:4096], nil, release, true)
	s := streamTestServer(t, map[string]string{"local": up.URL})
	plan := ttsPlan(streamingTarget("local", true))
	plan.SynthesisCache, plan.SynthesisCacheTTL = true, routing.DefaultSynthesisCacheTTL
	w := newRecordingWriter()
	_, res, streamed, err := s.runSynthesizeTo(context.Background(), plan, audio.SpeechRequest{Input: "hi", ResponseFormat: "wav"}, &speechSink{w: w, flush: w.Flush})
	if err == nil || !streamed || res.Cache != cacheMiss {
		t.Fatalf("streamed %v cache %q err %v; want a started break reported as a miss", streamed, res.Cache, err)
	}
	if _, hit, _ := s.speechCache.Get(context.Background(), speechcache.Key{Alias: plan.Alias, Provider: "local",
		Model: "chatterbox", Format: "wav", Text: "hi"}); hit {
		t.Fatal("a broken stream was stored in the cache")
	}
}
