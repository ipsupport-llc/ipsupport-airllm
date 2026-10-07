package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/routing"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/store"
)

func putAlias(s *Server, alias, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPut, "/api/admin/aliases/"+alias, strings.NewReader(body))
	req.SetPathValue("alias", alias)
	rec := httptest.NewRecorder()
	s.handleAdminPutAlias(rec, req)
	return rec
}

// TestPutAliasRejectsInvalidTargetOptions runs with no database at all: an
// invalid options object must be turned away before anything is written.
func TestPutAliasRejectsInvalidTargetOptions(t *testing.T) {
	s := &Server{}
	cases := map[string]string{
		"not an object":           `"fast"`,
		"array":                   `[1]`,
		"negative budget":         `{"timeout_ms":-1}`,
		"budget of wrong type":    `{"timeout_ms":"2s"}`,
		"flag of wrong type":      `{"fallback_on_auth":"yes"}`,
		"breaker not an object":   `{"breaker":"on"}`,
		"breaker zero failures":   `{"breaker":{"failures":0}}`,
		"breaker rate over one":   `{"breaker":{"error_rate":1.5}}`,
		"breaker zero cooldown":   `{"breaker":{"cooldown_ms":0}}`,
		"thinking other than off": `{"thinking":"on"}`,
		"thinking of wrong type":  `{"thinking":false}`,
		"models not a map":        `{"recognition_models":["long"]}`,
		"model left empty":        `{"recognition_models":{"en-US":""}}`,
		"one language twice":      `{"recognition_models":{"en-US":"long","EN_us":"telephony"}}`,
		"voices not a map":        `{"voices":["en-US-Neural2-F"]}`,
		"voice of wrong shape":    `{"voices":{"en-US-Neural2-F":"onyx"}}`,
		"unknown gender":          `{"voices":{"en-US-Neural2-F":{"voice":"onyx","gender":"f"}}}`,
		"default without voice":   `{"default_voices":{"en":{"model":"tts-1"}}}`,
		"default twice":           `{"default_voices":{"en":{"voice":"a"},"EN":{"voice":"b"}}}`,
	}
	for name, opts := range cases {
		t.Run(name, func(t *testing.T) {
			body := `{"targets":[{"priority":0,"provider":"p","upstream_model":"m","options":` + opts + `}]}`
			if rec := putAlias(s, "a", body); rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 (%s)", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestAliasTargetOptionsRoundTrip saves an alias with per-target options,
// reads it back through the admin list, and resolves it through the router
// the data plane uses.
func TestAliasTargetOptionsRoundTrip(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	prov := fmt.Sprintf("opts-prov-%d", time.Now().UnixNano())
	alias := "opts-alias-" + prov
	if _, err := pool.Exec(ctx, `INSERT INTO providers (name, kind, base_url, enabled, max_concurrency) VALUES ($1, 'openai', 'http://127.0.0.1:1', true, 1)`, prov); err != nil {
		t.Fatalf("seed provider: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM model_aliases WHERE alias = $1`, alias)
		_, _ = pool.Exec(context.Background(), `DELETE FROM providers WHERE name = $1`, prov)
	})
	s := &Server{st: &store.Store{PG: pool}, router: routing.NewRouter(&store.Store{PG: pool}), auditHook: func(context.Context, string, string, string, any) {}}

	body := fmt.Sprintf(`{"targets":[
		{"priority":0,"provider":%q,"upstream_model":"fast","options":{"timeout_ms":1500,"fallback_on_auth":true,"later_key":{"x":1}}},
		{"priority":10,"provider":%q,"upstream_model":"slow"}]}`, prov, prov)
	if rec := putAlias(s, alias, body); rec.Code != http.StatusOK {
		t.Fatalf("put: %d %s", rec.Code, rec.Body.String())
	}

	rec := httptest.NewRecorder()
	s.handleAdminAliases(rec, httptest.NewRequest(http.MethodGet, "/api/admin/aliases", nil))
	var list struct {
		Aliases []struct {
			Alias   string `json:"alias"`
			Targets []struct {
				UpstreamModel string          `json:"upstream_model"`
				Options       json.RawMessage `json:"options"`
			} `json:"targets"`
		} `json:"aliases"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	got := map[string]map[string]any{}
	for _, a := range list.Aliases {
		if a.Alias != alias {
			continue
		}
		for _, tg := range a.Targets {
			var o map[string]any
			if err := json.Unmarshal(tg.Options, &o); err != nil {
				t.Fatalf("options of %s are not an object: %s", tg.UpstreamModel, tg.Options)
			}
			got[tg.UpstreamModel] = o
		}
	}
	if o := got["fast"]; o["timeout_ms"] != float64(1500) || o["fallback_on_auth"] != true || o["later_key"] == nil {
		t.Errorf("fast target options = %v, want the saved object back, unknown keys included", o)
	}
	if o, ok := got["slow"]; !ok || len(o) != 0 {
		t.Errorf("slow target options = %v, want an empty object", o)
	}

	plan, err := s.router.Resolve(ctx, alias, false)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	fast := plan.Tiers[0][0].Options
	if fast.TimeoutMS == nil || *fast.TimeoutMS != 1500 || fast.FallbackOnAuth == nil || !*fast.FallbackOnAuth {
		t.Errorf("resolved options = %+v, want timeout_ms=1500 fallback_on_auth=true", fast)
	}
	if tier := plan.Tiers[1][0].Tier; tier != 10 {
		t.Errorf("second tier's Tier = %d, want its configured priority 10 — stable when other tiers come and go", tier)
	}
	if slow := plan.Tiers[1][0].Options; slow.TimeoutMS != nil || slow.FallbackOnAuth != nil {
		t.Errorf("resolved options for the unset target = %+v, want nothing set", slow)
	}
}

func TestFailoverDefaultsRoundTrip(t *testing.T) {
	s := &Server{auditHook: func(context.Context, string, string, string, any) {}}
	bad := httptest.NewRequest(http.MethodPut, "/api/admin/failover", strings.NewReader(`{"timeout_ms":-5}`))
	rec := httptest.NewRecorder()
	s.handleAdminPutFailover(rec, bad)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("negative default budget: status = %d, want 400 before anything is saved", rec.Code)
	}
	rec = httptest.NewRecorder()
	s.handleAdminPutFailover(rec, httptest.NewRequest(http.MethodPut, "/api/admin/failover", strings.NewReader(`{"breaker":{"min_requests":-1}}`)))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("negative breaker volume: status = %d, want 400 before anything is saved", rec.Code)
	}

	pool := testPool(t)
	ctx := context.Background()
	var prev []byte
	_ = pool.QueryRow(ctx, `SELECT value FROM settings WHERE name = 'failover'`).Scan(&prev)
	t.Cleanup(func() {
		if prev == nil {
			_, _ = pool.Exec(context.Background(), `DELETE FROM settings WHERE name = 'failover'`)
		} else {
			_, _ = pool.Exec(context.Background(), `UPDATE settings SET value = $1 WHERE name = 'failover'`, prev)
		}
	})
	s.st = &store.Store{PG: pool}

	rec = httptest.NewRecorder()
	s.handleAdminPutFailover(rec, httptest.NewRequest(http.MethodPut, "/api/admin/failover", strings.NewReader(`{"timeout_ms":2500,"fallback_on_auth":true,"breaker":{"enabled":true,"cooldown_ms":5000}}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("put: %d %s", rec.Code, rec.Body.String())
	}

	// A fresh server, as another replica or a restart would see it.
	fresh := &Server{st: &store.Store{PG: pool}}
	fresh.loadFailover(ctx)
	rec = httptest.NewRecorder()
	fresh.handleAdminGetFailover(rec, httptest.NewRequest(http.MethodGet, "/api/admin/failover", nil))
	// unavailable_initial_ms/unavailable_max_ms are always present: unlike
	// the breaker, that mechanism has no enable flag and loadFailover fills
	// in its built-in default whenever a save didn't set them.
	if got := strings.TrimSpace(rec.Body.String()); got != `{"timeout_ms":2500,"fallback_on_auth":true,"breaker":{"enabled":true,"cooldown_ms":5000},"unavailable_initial_ms":200,"unavailable_max_ms":25600}` {
		t.Errorf("get = %s, want the saved defaults", got)
	}
}

// TestPutAliasRejectsAnOutOfRangeAffinityTTL runs with no database: the TTL
// is checked before anything is written.
func TestPutAliasRejectsAnOutOfRangeAffinityTTL(t *testing.T) {
	s := &Server{}
	for _, ttl := range []string{"-1", "604801", `"4h"`} {
		body := `{"session_affinity":true,"session_affinity_ttl_s":` + ttl + `,"targets":[]}`
		if rec := putAlias(s, "a", body); rec.Code != http.StatusBadRequest {
			t.Errorf("ttl %s: status = %d, want 400 (%s)", ttl, rec.Code, rec.Body.String())
		}
	}
}

// TestAliasSessionAffinityRoundTrip saves an alias with affinity on, reads it
// back through the admin list and resolves it through the router.
func TestAliasSessionAffinityRoundTrip(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	prov := fmt.Sprintf("aff-prov-%d", time.Now().UnixNano())
	alias, plain := "aff-alias-"+prov, "aff-plain-"+prov
	if _, err := pool.Exec(ctx, `INSERT INTO providers (name, kind, base_url, enabled, max_concurrency) VALUES ($1, 'openai', 'http://127.0.0.1:1', true, 1)`, prov); err != nil {
		t.Fatalf("seed provider: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM model_aliases WHERE alias = ANY($1)`, []string{alias, plain})
		_, _ = pool.Exec(context.Background(), `DELETE FROM providers WHERE name = $1`, prov)
	})
	s := &Server{st: &store.Store{PG: pool}, router: routing.NewRouter(&store.Store{PG: pool}), auditHook: func(context.Context, string, string, string, any) {}}

	target := fmt.Sprintf(`"targets":[{"priority":0,"provider":%q,"upstream_model":"m"}]`, prov)
	if rec := putAlias(s, alias, `{"session_affinity":true,"session_affinity_ttl_s":7200,`+target+`}`); rec.Code != http.StatusOK {
		t.Fatalf("put: %d %s", rec.Code, rec.Body.String())
	}
	if rec := putAlias(s, plain, `{`+target+`}`); rec.Code != http.StatusOK {
		t.Fatalf("put plain: %d %s", rec.Code, rec.Body.String())
	}

	rec := httptest.NewRecorder()
	s.handleAdminAliases(rec, httptest.NewRequest(http.MethodGet, "/api/admin/aliases", nil))
	var list struct {
		Aliases []struct {
			Alias           string `json:"alias"`
			SessionAffinity bool   `json:"session_affinity"`
			TTL             int    `json:"session_affinity_ttl_s"`
		} `json:"aliases"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	for _, a := range list.Aliases {
		switch a.Alias {
		case alias:
			if !a.SessionAffinity || a.TTL != 7200 {
				t.Errorf("listed affinity=%v ttl=%d, want true and 7200", a.SessionAffinity, a.TTL)
			}
		case plain:
			if a.SessionAffinity || a.TTL != 0 {
				t.Errorf("plain alias listed affinity=%v ttl=%d, want it off by default", a.SessionAffinity, a.TTL)
			}
		}
	}

	plan, err := s.router.Resolve(ctx, alias, false)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !plan.SessionAffinity || plan.SessionAffinityTTL != 2*time.Hour {
		t.Errorf("resolved affinity=%v ttl=%v, want true and 2h", plan.SessionAffinity, plan.SessionAffinityTTL)
	}
	plan, err = s.router.Resolve(ctx, plain, false)
	if err != nil {
		t.Fatalf("resolve plain: %v", err)
	}
	if plan.SessionAffinity || plan.SessionAffinityTTL != routing.DefaultAffinityTTL {
		t.Errorf("plain alias resolved affinity=%v ttl=%v, want off with the default TTL", plan.SessionAffinity, plan.SessionAffinityTTL)
	}
}
