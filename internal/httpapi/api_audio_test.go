package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/audio"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/ledger"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/limits"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/metrics"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/policy"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/pricing"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/providers"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/routing"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/speechcache"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/store"
)

// These drive the audio routes from the outside: aliases saved through the
// admin API, resolved by the real router, served by real providers talking to
// in-process upstreams, and the assertions are on the HTTP reply and on what
// the ledger recorded. They need the test database.

// audioRig is a Server with what the audio routes touch, and the ledger it
// records into. The rolling counters point at a closed port: the limiter's
// writes fail and are logged, which is what a Redis outage does in
// production too.
type audioRig struct {
	*Server
	led     *ledger.Ledger
	stop    sync.Once
	aliases []string
}

// flushLedger writes every recorded row; the ledger takes no more after it.
func (r *audioRig) flushLedger() { r.stop.Do(r.led.Stop) }

func newAudioRig(t *testing.T, provs ...providers.Provider) *audioRig {
	t.Helper()
	pool := testPool(t)
	st := &store.Store{PG: pool}
	reg := providers.NewRegistry()
	for _, p := range provs {
		reg.Register(p, 0)
	}
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	led := ledger.New(st)
	led.Start()
	s := &Server{
		st: st, router: routing.NewRouter(st), metrics: metrics.New(), pricing: pricing.New(),
		limiter: limits.New(rdb), ledger: led,
		auditHook: func(context.Context, string, string, string, any) {},
	}
	s.regPtr.Store(reg)
	rig := &audioRig{Server: s, led: led}
	// Registered before any alias is seeded, so it runs after their own
	// cleanups: the rows are written, then removed, leaving the ledger as the
	// other tests expect to find it.
	t.Cleanup(func() {
		rig.flushLedger()
		_, _ = pool.Exec(context.Background(), `DELETE FROM usage_ledger WHERE alias = ANY($1)`, rig.aliases)
	})
	return rig
}

// seedAudioAlias stores providers of the given kinds and an alias whose
// targets body is saved through the admin API, with transcript scanning off.
func seedAudioAlias(t *testing.T, s *audioRig, alias string, kinds map[string]string, targets string) {
	t.Helper()
	s.aliases = append(s.aliases, alias)
	ctx := context.Background()
	for name, kind := range kinds {
		if _, err := s.st.PG.Exec(ctx, `INSERT INTO providers (name, kind, base_url, enabled, config) VALUES ($1, $2, 'http://127.0.0.1:1', true, '{"project":"acme"}')`, name, kind); err != nil {
			t.Fatalf("seed provider %s: %v", name, err)
		}
	}
	t.Cleanup(func() {
		_, _ = s.st.PG.Exec(context.Background(), `DELETE FROM model_aliases WHERE alias = $1`, alias)
		for name := range kinds {
			_, _ = s.st.PG.Exec(context.Background(), `DELETE FROM providers WHERE name = $1`, name)
		}
	})
	if rec := putAlias(s.Server, alias, `{"targets":`+targets+`}`); rec.Code != http.StatusOK {
		t.Fatalf("put alias: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := s.st.PG.Exec(ctx, `UPDATE model_aliases SET dlp_audio_scan = false WHERE alias = $1`, alias); err != nil {
		t.Fatalf("turn scanning off: %v", err)
	}
}

func asKey(r *http.Request, aliases ...string) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), keyCtxKey, authedKey{Policy: policy.KeyPolicy{AllowedModels: aliases}}))
}

// transcriptionRequest builds a multipart upload; fields repeat in order.
func transcriptionRequest(t *testing.T, fields [][2]string) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, f := range fields {
		_ = mw.WriteField(f[0], f[1])
	}
	fw, _ := mw.CreateFormFile("file", "u.wav")
	_, _ = fw.Write([]byte("RIFF....WAVE"))
	_ = mw.Close()
	r := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", &buf)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	return r
}

func TestTranscriptionRouteAnswersVerboseFromAGoogleTierAndPricesTheModelThatRan(t *testing.T) {
	up, calls := fakeGoogleSpeech(t, func(speechCall) (int, string) { return http.StatusOK, googleRecognized })
	suffix := fmt.Sprint(time.Now().UnixNano())
	gstt, alias := "gstt-"+suffix, "voice-stt-"+suffix
	s := newAudioRig(t, newGoogleSpeech(gstt, up.URL))
	seedAudioAlias(t, s, alias, map[string]string{gstt: providers.KindGoogleSpeech},
		fmt.Sprintf(`[{"priority":0,"provider":%q,"upstream_model":"long","options":{"recognition_models":{"en-US":"telephony"}}}]`, gstt))
	// Google's standard rate, $0.016 a minute, per million seconds.
	s.pricing.Set(gstt, "telephony", pricing.Price{Unit: pricing.UnitAudioSecond, InputPer1M: 266.67})

	rec := httptest.NewRecorder()
	s.handleAudioTranscriptions(rec, asKey(transcriptionRequest(t, [][2]string{
		{"model", alias}, {"language", "en-us"}, {"response_format", "verbose_json"},
		{"alternative_languages[]", "es"}, {"alternative_languages[]", "de-DE,uk"},
	}), alias))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := map[string]any{"task": "transcribe", "text": "hello there how are you", "language": "en-US", "confidence": 0.8, "duration": float64(3)}
	for k, v := range want {
		if fmt.Sprint(got[k]) != fmt.Sprint(v) {
			t.Errorf("%s = %v, want %v (reply %s)", k, got[k], v, rec.Body.String())
		}
	}
	if langs := calls()[0].body.Config.LanguageCodes; !slices.Equal(langs, []string{"en-US", "es-ES", "de-DE", "uk-UA"}) {
		t.Errorf("Google got languages %v, want the primary then every alternative spelling", langs)
	}

	s.flushLedger()
	var model string
	var cost float64
	if err := s.st.PG.QueryRow(context.Background(),
		`SELECT upstream_model, cost_usd::float8 FROM usage_ledger WHERE alias = $1`, alias).Scan(&model, &cost); err != nil {
		t.Fatalf("ledger row: %v", err)
	}
	if model != "telephony" || cost != 0.0008 {
		t.Errorf("ledger model=%q cost=%v, want telephony at 3 billed seconds = $0.0008", model, cost)
	}
}

func TestTranscriptionRouteKeepsThePlainReplyByDefault(t *testing.T) {
	suffix := fmt.Sprint(time.Now().UnixNano())
	mock, alias := "mock-stt-"+suffix, "plain-stt-"+suffix
	s := newAudioRig(t, providers.NewMock(mock))
	seedAudioAlias(t, s, alias, map[string]string{mock: "mock"}, fmt.Sprintf(`[{"priority":0,"provider":%q,"upstream_model":"m"}]`, mock))

	rec := httptest.NewRecorder()
	s.handleAudioTranscriptions(rec, asKey(transcriptionRequest(t, [][2]string{{"model", alias}, {"language", "en"}}), alias))
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if rec.Code != http.StatusOK || len(got) != 1 || got["text"] == nil {
		t.Errorf("status %d reply %s, want only the text", rec.Code, rec.Body.String())
	}
}

func TestCapabilitiesRouteReportsTheFirstTiersRecognitionLanguages(t *testing.T) {
	suffix := fmt.Sprint(time.Now().UnixNano())
	gstt, mock := "gstt-"+suffix, "mock-"+suffix
	googleFirst, mockFirst := "cloud-stt-"+suffix, "local-stt-"+suffix
	s := newAudioRig(t, newGoogleSpeech(gstt, "http://127.0.0.1:1"), providers.NewMock(mock))
	kinds := map[string]string{gstt: providers.KindGoogleSpeech, mock: "mock"}
	seedAudioAlias(t, s, googleFirst, kinds, fmt.Sprintf(`[{"priority":0,"provider":%q,"upstream_model":"long"},{"priority":1,"provider":%q,"upstream_model":"m"}]`, gstt, mock))
	seedAudioAlias(t, s, mockFirst, nil, fmt.Sprintf(`[{"priority":0,"provider":%q,"upstream_model":"m"},{"priority":1,"provider":%q,"upstream_model":"long"}]`, mock, gstt))

	capabilities := func(alias string, allowed ...string) (int, []string) {
		rec := httptest.NewRecorder()
		s.handleAudioCapabilities(rec, asKey(httptest.NewRequest(http.MethodGet, "/v1/audio/capabilities?model="+alias, nil), allowed...))
		var body struct {
			Model       string `json:"model"`
			Recognition struct {
				Languages []string `json:"languages"`
			} `json:"recognition"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return rec.Code, body.Recognition.Languages
	}

	if code, langs := capabilities(googleFirst, googleFirst); code != http.StatusOK || !slices.Contains(langs, "uk") || !slices.Contains(langs, "en") || slices.Contains(langs, "en-US") {
		t.Errorf("google-first alias: status %d languages %v, want Google's set as primary subtags", code, langs)
	}
	if code, langs := capabilities(mockFirst, mockFirst); code != http.StatusOK || !slices.Equal(langs, []string{"en", "es"}) {
		t.Errorf("mock-first alias: status %d languages %v, want the first tier's [en es], not the later Google tier's", code, langs)
	}
	if code, _ := capabilities(googleFirst, mockFirst); code != http.StatusForbidden {
		t.Errorf("alias the key may not use: status %d, want 403", code)
	}
}

func speechRequest(body string) *http.Request {
	return httptest.NewRequest(http.MethodPost, "/v1/audio/speech", strings.NewReader(body))
}

func TestSpeechRouteAnswersWAVFromAGoogleTierAndPricesTheVoiceFamily(t *testing.T) {
	up, _ := fakeGoogleTTS(t, func(ttsCall) (int, string) { return http.StatusOK, googleSynthesized(pcmWAV(24000, 1)) })
	suffix := fmt.Sprint(time.Now().UnixNano())
	gtts, alias := "gtts-"+suffix, "voice-tts-"+suffix
	s := newAudioRig(t, newGoogleTTS(gtts, up.URL))
	seedAudioAlias(t, s, alias, map[string]string{gtts: providers.KindGoogleTTS},
		fmt.Sprintf(`[{"priority":0,"provider":%q,"upstream_model":"neural2"}]`, gtts))
	// Chirp 3: HD at Google's $30 per million characters.
	s.pricing.Set(gtts, "chirp3-hd", pricing.Price{Unit: pricing.UnitTextChar, InputPer1M: 30})

	rec := httptest.NewRecorder()
	s.handleAudioSpeech(rec, asKey(speechRequest(fmt.Sprintf(`{"model":%q,"input":"Hello there","voice":"en-US-Chirp3-HD-Charon"}`, alias)), alias))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "audio/wav" {
		t.Errorf("content type = %q, want audio/wav by default", ct)
	}
	if rate, ok := audio.WAVSampleRate(rec.Body.Bytes()); !ok || rate != 24000 {
		t.Errorf("sample rate = %d (readable %v), want Google's 24000 in the WAV header", rate, ok)
	}

	s.flushLedger()
	var model string
	var cost float64
	if err := s.st.PG.QueryRow(context.Background(),
		`SELECT upstream_model, cost_usd::float8 FROM usage_ledger WHERE alias = $1`, alias).Scan(&model, &cost); err != nil {
		t.Fatalf("ledger row: %v", err)
	}
	if model != "chirp3-hd" || cost != 0.00033 {
		t.Errorf("ledger model=%q cost=%v, want chirp3-hd at 11 characters = $0.00033", model, cost)
	}
}

func TestCapabilitiesRouteReportsTheFirstTiersVoices(t *testing.T) {
	up, _ := fakeGoogleTTS(t, func(ttsCall) (int, string) { return http.StatusOK, "{}" })
	suffix := fmt.Sprint(time.Now().UnixNano())
	gtts, piper := "gtts-"+suffix, "piper-"+suffix
	cloud, local := "cloud-tts-"+suffix, "local-tts-"+suffix
	s := newAudioRig(t, newGoogleTTS(gtts, up.URL), providers.NewOpenAICompat(piper, "openai", "http://127.0.0.1:1", ""))
	kinds := map[string]string{gtts: providers.KindGoogleTTS, piper: "openai"}
	seedAudioAlias(t, s, cloud, kinds, fmt.Sprintf(`[{"priority":0,"provider":%q,"upstream_model":"neural2"}]`, gtts))
	seedAudioAlias(t, s, local, nil, fmt.Sprintf(`[
		{"priority":0,"provider":%q,"upstream_model":"piper","options":{"voices":{
			"ru_RU-irina-medium":{"gender":"female"},
			"en_US-ryan-high":{"gender":"male"},
			"narrator":{"voice":"en_US-lessac-medium","language":"en-US"}}}},
		{"priority":1,"provider":%q,"upstream_model":"neural2"}]`, piper, gtts))

	voices := func(alias string) ([]audio.Voice, bool) {
		rec := httptest.NewRecorder()
		s.handleAudioCapabilities(rec, asKey(httptest.NewRequest(http.MethodGet, "/v1/audio/capabilities?model="+alias, nil), alias))
		var body struct {
			Recognition *struct{} `json:"recognition"`
			Synthesis   struct {
				Voices []audio.Voice `json:"voices"`
			} `json:"synthesis"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d reply %s", alias, rec.Code, rec.Body.String())
		}
		return body.Synthesis.Voices, body.Recognition != nil
	}

	got, recognises := voices(cloud)
	if recognises {
		t.Error("Google-first alias: recognition reported for a first tier that only synthesizes")
	}
	if want := []audio.Voice{
		{ID: "en-US-Chirp3-HD-Charon", Language: "en-US", Gender: "male"},
		{ID: "es-ES-Neural2-A", Language: "es-ES", Gender: "female"},
	}; !slices.Equal(got, want) {
		t.Errorf("Google-first alias: voices %+v, want Google's catalogue %+v", got, want)
	}
	got, _ = voices(local)
	if want := []audio.Voice{
		{ID: "en_US-ryan-high", Language: "en-US", Gender: "male"},
		{ID: "narrator", Language: "en-US"},
		{ID: "ru_RU-irina-medium", Language: "ru-RU", Gender: "female"},
	}; !slices.Equal(got, want) {
		t.Errorf("Piper-first alias: voices %+v, want the first tier's own voices %+v", got, want)
	}
}

func TestSpeechRouteServesARepeatFromTheCacheAndMetersItAsCached(t *testing.T) {
	up, calls := fakeOpenAISpeech(t, "audio/wav", pcmWAV(22050, 1))
	suffix := fmt.Sprint(time.Now().UnixNano())
	piper, alias := "piper-"+suffix, "voice-tts-"+suffix
	s := newAudioRig(t, providers.NewOpenAICompat(piper, "openai", up.URL, ""))
	s.speechCache = speechcache.New(nil)
	seedAudioAlias(t, s, alias, map[string]string{piper: "openai"},
		fmt.Sprintf(`[{"priority":0,"provider":%q,"upstream_model":"tts-1"}]`, piper))
	if rec := putAlias(s.Server, alias, fmt.Sprintf(`{"synthesis_cache":true,"dlp_audio_scan":false,"targets":[{"priority":0,"provider":%q,"upstream_model":"tts-1"}]}`, piper)); rec.Code != http.StatusOK {
		t.Fatalf("turn the cache on: %d %s", rec.Code, rec.Body.String())
	}
	s.pricing.Set(piper, "tts-1", pricing.Price{Unit: pricing.UnitTextChar, InputPer1M: 15})
	logs := captureLogs(t)

	var replies [][]byte
	for range 2 {
		rec := httptest.NewRecorder()
		s.handleAudioSpeech(rec, asKey(speechRequest(fmt.Sprintf(`{"model":%q,"input":"Hello there","voice":"alloy"}`, alias)), alias))
		if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "audio/wav" {
			t.Fatalf("status = %d, content type %q: %s", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
		}
		replies = append(replies, rec.Body.Bytes())
	}
	if n := len(calls()); n != 1 {
		t.Errorf("upstream asked %d times, want the repeat answered from the cache", n)
	}
	if !bytes.Equal(replies[0], replies[1]) {
		t.Error("the cached reply differs from the synthesized one")
	}

	s.flushLedger()
	rows, err := s.st.PG.Query(context.Background(),
		`SELECT cached, cost_usd::float8, attempts, provider_name, upstream_model, status FROM usage_ledger WHERE alias = $1 ORDER BY ts, id`, alias)
	if err != nil {
		t.Fatalf("ledger rows: %v", err)
	}
	defer rows.Close()
	type row struct {
		cached          bool
		cost            float64
		attempts        int
		provider, model string
		status          int
	}
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.cached, &r.cost, &r.attempts, &r.provider, &r.model, &r.status); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, r)
	}
	want := []row{
		{false, 0.000165, 1, piper, "tts-1", http.StatusOK},
		{true, 0, 0, piper, "tts-1", http.StatusOK},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("ledger = %+v, want the synthesis at 11 characters and then a cached row at no cost and no attempt: %+v", got, want)
	}

	var outcomes []string
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var rec struct {
			Msg   string `json:"msg"`
			Cache string `json:"cache"`
		}
		if json.Unmarshal([]byte(line), &rec) == nil && rec.Msg == "request completed" {
			outcomes = append(outcomes, rec.Cache)
		}
	}
	if fmt.Sprint(outcomes) != "[miss hit]" {
		t.Errorf("request log cache outcomes = %v, want [miss hit]", outcomes)
	}
}

func TestAliasSynthesisCacheSettingsRoundTripAndDefaultOff(t *testing.T) {
	suffix := fmt.Sprint(time.Now().UnixNano())
	piper, plain, cached := "piper-"+suffix, "plain-tts-"+suffix, "cached-tts-"+suffix
	s := newAudioRig(t, providers.NewOpenAICompat(piper, "openai", "http://127.0.0.1:1", ""))
	seedAudioAlias(t, s, plain, map[string]string{piper: "openai"},
		fmt.Sprintf(`[{"priority":0,"provider":%q,"upstream_model":"tts-1"}]`, piper))
	s.aliases = append(s.aliases, cached)
	t.Cleanup(func() {
		_, _ = s.st.PG.Exec(context.Background(), `DELETE FROM model_aliases WHERE alias = $1`, cached)
	})
	if rec := putAlias(s.Server, cached, fmt.Sprintf(`{"synthesis_cache":true,"synthesis_cache_ttl_s":172800,"targets":[{"priority":0,"provider":%q,"upstream_model":"tts-1"}]}`, piper)); rec.Code != http.StatusOK {
		t.Fatalf("put alias: %d %s", rec.Code, rec.Body.String())
	}

	ctx := context.Background()
	for alias, want := range map[string]struct {
		on  bool
		ttl time.Duration
	}{plain: {false, routing.DefaultSynthesisCacheTTL}, cached: {true, 48 * time.Hour}} {
		plan, err := s.router.Resolve(ctx, alias, false)
		if err != nil {
			t.Fatalf("resolve %s: %v", alias, err)
		}
		if plan.SynthesisCache != want.on || plan.SynthesisCacheTTL != want.ttl {
			t.Errorf("%s: cache=%v ttl=%v, want %v %v", alias, plan.SynthesisCache, plan.SynthesisCacheTTL, want.on, want.ttl)
		}
	}

	rec := httptest.NewRecorder()
	s.handleAdminAliases(rec, httptest.NewRequest(http.MethodGet, "/api/admin/aliases", nil))
	var list struct {
		Aliases []struct {
			Alias              string `json:"alias"`
			SynthesisCache     bool   `json:"synthesis_cache"`
			SynthesisCacheTTLS int    `json:"synthesis_cache_ttl_s"`
		} `json:"aliases"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	found := 0
	for _, a := range list.Aliases {
		switch a.Alias {
		case plain:
			found++
			if a.SynthesisCache || a.SynthesisCacheTTLS != 0 {
				t.Errorf("plain alias lists cache=%v ttl=%d, want off and 0", a.SynthesisCache, a.SynthesisCacheTTLS)
			}
		case cached:
			found++
			if !a.SynthesisCache || a.SynthesisCacheTTLS != 172800 {
				t.Errorf("cached alias lists cache=%v ttl=%d, want on and 172800", a.SynthesisCache, a.SynthesisCacheTTLS)
			}
		}
	}
	if found != 2 {
		t.Errorf("found %d of the two aliases in the list", found)
	}
}
