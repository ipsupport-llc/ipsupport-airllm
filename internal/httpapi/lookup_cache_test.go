package httpapi

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/apikey"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/auth"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/config"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/limits"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/pricing"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/providers"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/store"
)

// These are the seam tests for the API-key and alias lookup cache. Requests go
// through the full HTTP handler; the server's database connection runs through
// dbProxy, which a test can cut to simulate an outage or restart while the
// fixtures are still written through a direct pool (standing in for the
// admin API of another replica). Time is a fake clock, so TTLs are crossed
// without sleeping.

const (
	testCacheTTL      = 30 * time.Second
	testCacheMaxStale = 5 * time.Minute
)

// dbProxy forwards TCP to the test database until cut.
type dbProxy struct {
	ln       net.Listener
	target   string
	down     atomic.Bool
	upstream atomic.Int64 // bytes the gateway sent towards the database

	mu    sync.Mutex
	conns []net.Conn
}

func newDBProxy(t *testing.T, target string) *dbProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	p := &dbProxy{ln: ln, target: target}
	t.Cleanup(func() { _ = ln.Close(); p.cut() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			if p.down.Load() {
				_ = c.Close()
				continue
			}
			go p.pipe(c)
		}
	}()
	return p
}

func (p *dbProxy) pipe(c net.Conn) {
	u, err := net.Dial("tcp", p.target)
	if err != nil {
		_ = c.Close()
		return
	}
	p.mu.Lock()
	if p.down.Load() { // cut while this one was dialling
		p.mu.Unlock()
		_ = c.Close()
		_ = u.Close()
		return
	}
	p.conns = append(p.conns, c, u)
	p.mu.Unlock()
	go func() {
		_, _ = io.Copy(countingWriter{u, &p.upstream}, c)
		_ = u.Close()
	}()
	_, _ = io.Copy(c, u)
	_ = c.Close()
}

// cut drops every open connection and refuses new ones until restore.
func (p *dbProxy) cut() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.down.Store(true)
	for _, c := range p.conns {
		_ = c.Close()
	}
	p.conns = nil
}

func (p *dbProxy) restore() { p.down.Store(false) }

type countingWriter struct {
	w io.Writer
	n *atomic.Int64
}

func (c countingWriter) Write(b []byte) (int, error) {
	n, err := c.w.Write(b)
	c.n.Add(int64(n))
	return n, err
}

// noSession rejects every control-plane request as unauthenticated.
type noSession struct{}

func (noSession) Authenticate(*http.Request) (auth.Principal, error) {
	return auth.Principal{}, fmt.Errorf("no session")
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// cacheFixture is one gateway instance with its own key, alias and provider.
type cacheFixture struct {
	srv     *Server
	direct  *pgxpool.Pool
	proxy   *dbProxy
	clock   *fakeClock
	token   string
	keyID   string
	alias   string
	userSub string
}

func newCacheFixture(t *testing.T, principal *auth.Principal) *cacheFixture {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	rurl := os.Getenv("TEST_REDIS_URL")
	if dsn == "" || rurl == "" {
		t.Skip("TEST_DATABASE_URL and TEST_REDIS_URL not set; skipping lookup cache test")
	}
	direct := testPool(t)
	ctx := context.Background()

	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	proxy := newDBProxy(t, u.Host)
	u.Host = proxy.ln.Addr().String()
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatalf("proxied pool: %v", err)
	}
	t.Cleanup(pool.Close)
	ropt, err := redis.ParseURL(rurl)
	if err != nil {
		t.Fatalf("redis url: %v", err)
	}
	rdb := redis.NewClient(ropt)
	t.Cleanup(func() { _ = rdb.Close() })

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	f := &cacheFixture{
		direct:  direct,
		proxy:   proxy,
		clock:   &fakeClock{t: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)},
		token:   "air_test_cache_" + suffix,
		alias:   "cache-alias-" + suffix,
		userSub: "cache-user-" + suffix,
	}
	provider := "cache-mock-" + suffix

	mustExec := func(sql string, args ...any) {
		t.Helper()
		if _, err := direct.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %s: %v", sql, err)
		}
	}
	mustExec(`INSERT INTO providers (name, kind) VALUES ($1, 'mock')`, provider)
	mustExec(`INSERT INTO model_aliases (alias, protocol, expose_backend_headers) VALUES ($1, 'openai', true)`, f.alias)
	mustExec(`INSERT INTO alias_targets (alias, priority, provider_name, upstream_model, upstream_protocol, display_label)
		VALUES ($1, 0, $2, 'mock-gpt', 'openai', 'label-one')`, f.alias, provider)
	var userID string
	if err := direct.QueryRow(ctx, `INSERT INTO users (subject) VALUES ($1) RETURNING id::text`, f.userSub).Scan(&userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if err := direct.QueryRow(ctx, `
		INSERT INTO api_keys (user_id, hash, prefix, last4, policy_snapshot)
		VALUES ($1, $2, 'air_test', 'test', $3) RETURNING id::text`,
		userID, apikey.Hash(f.token), fmt.Sprintf(`{"allowed_models":[%q]}`, f.alias),
	).Scan(&f.keyID); err != nil {
		t.Fatalf("insert key: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = direct.Exec(bg, `DELETE FROM users WHERE id = $1`, userID)
		_, _ = direct.Exec(bg, `DELETE FROM model_aliases WHERE alias = $1`, f.alias)
		_, _ = direct.Exec(bg, `DELETE FROM providers WHERE name = $1`, provider)
	})

	reg := providers.NewRegistry()
	reg.Register(providers.NewMock(provider), 0)
	deps := Deps{
		Providers: reg,
		Limiter:   limits.New(rdb),
		Pricing:   pricing.New(),
		Now:       f.clock.Now,
	}
	deps.Auth = noSession{}
	if principal != nil {
		deps.Auth = &fakeAuth{principal: *principal}
	}
	cfg := &config.Config{Env: "dev", LookupCacheTTL: testCacheTTL, LookupCacheMaxStale: testCacheMaxStale}
	f.srv = NewServer(cfg, &store.Store{PG: pool, RDB: rdb}, deps)
	return f
}

// startLedger runs the usage ledger's writers, which talk to the database in
// the background. Left stopped, ledger entries just queue.
func (f *cacheFixture) startLedger(t *testing.T) {
	f.srv.Ledger().Start()
	t.Cleanup(f.srv.Ledger().Stop)
}

// chat sends one non-streaming chat request with token and returns the
// response.
func (f *cacheFixture) chat(t *testing.T, token string) *httptest.ResponseRecorder {
	t.Helper()
	body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}]}`, f.alias)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, req)
	return rec
}

func wantStatus(t *testing.T, rec *httptest.ResponseRecorder, want int, when string) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("%s: status %d, want %d; body: %s", when, rec.Code, want, rec.Body.String())
	}
}

func TestSeenKeyAndAliasKeepWorkingThroughADatabaseOutage(t *testing.T) {
	f := newCacheFixture(t, nil)
	f.startLedger(t)
	wantStatus(t, f.chat(t, f.token), http.StatusOK, "warm-up")

	f.proxy.cut()
	start := time.Now()
	wantStatus(t, f.chat(t, f.token), http.StatusOK, "database down, inside TTL")

	// Past the TTL the gateway asks again, finds the database still gone and
	// keeps serving what it last knew.
	f.clock.Advance(testCacheTTL + time.Second)
	wantStatus(t, f.chat(t, f.token), http.StatusOK, "database down, past TTL")
	f.clock.Advance(time.Minute)
	wantStatus(t, f.chat(t, f.token), http.StatusOK, "database down, a minute later")
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("outage requests took %v — something waited on the database or the ledger", elapsed)
	}

	// Beyond the staleness bound the last answer is no longer trusted.
	f.clock.Advance(testCacheMaxStale)
	wantStatus(t, f.chat(t, f.token), http.StatusUnauthorized, "database down, past max stale")

	// And once the database is back the same key works again.
	f.proxy.restore()
	wantStatus(t, f.chat(t, f.token), http.StatusOK, "database back")
}

func TestUnknownKeyIsRejectedWhileTheDatabaseIsDown(t *testing.T) {
	f := newCacheFixture(t, nil)
	wantStatus(t, f.chat(t, f.token), http.StatusOK, "warm-up")
	f.proxy.cut()
	wantStatus(t, f.chat(t, "air_test_never_seen"), http.StatusUnauthorized, "unknown key, database down")
}

func TestCacheHitAsksTheDatabaseNothing(t *testing.T) {
	f := newCacheFixture(t, nil)
	wantStatus(t, f.chat(t, f.token), http.StatusOK, "warm-up")
	// The ledger is left stopped, so the request path is the only thing that
	// could talk to the database.
	before := f.proxy.upstream.Load()
	wantStatus(t, f.chat(t, f.token), http.StatusOK, "cache hit")
	if sent := f.proxy.upstream.Load() - before; sent != 0 {
		t.Errorf("a cache hit sent %d bytes to the database, want none", sent)
	}
}

func TestRevokedKeyStopsWithinTTL(t *testing.T) {
	f := newCacheFixture(t, nil)
	wantStatus(t, f.chat(t, f.token), http.StatusOK, "warm-up")

	// Revoked elsewhere (another replica's admin API).
	if _, err := f.direct.Exec(context.Background(), `UPDATE api_keys SET status = 'revoked' WHERE id = $1`, f.keyID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	f.clock.Advance(testCacheTTL + time.Second)
	wantStatus(t, f.chat(t, f.token), http.StatusUnauthorized, "revoked, past TTL")

	// A revocation is not undone by a later outage: the entry is gone.
	f.proxy.cut()
	wantStatus(t, f.chat(t, f.token), http.StatusUnauthorized, "revoked, then database down")
}

func TestEditedAliasTakesEffectWithinTTL(t *testing.T) {
	f := newCacheFixture(t, nil)
	rec := f.chat(t, f.token)
	wantStatus(t, rec, http.StatusOK, "warm-up")
	if got := rec.Header().Get("X-Backend-Model"); got != "label-one" {
		t.Fatalf("X-Backend-Model = %q, want label-one", got)
	}

	if _, err := f.direct.Exec(context.Background(), `UPDATE alias_targets SET display_label = 'label-two' WHERE alias = $1`, f.alias); err != nil {
		t.Fatalf("edit alias: %v", err)
	}
	f.clock.Advance(testCacheTTL + time.Second)
	rec = f.chat(t, f.token)
	wantStatus(t, rec, http.StatusOK, "edited, past TTL")
	if got := rec.Header().Get("X-Backend-Model"); got != "label-two" {
		t.Errorf("X-Backend-Model = %q, want label-two once the TTL has passed", got)
	}

	if _, err := f.direct.Exec(context.Background(), `DELETE FROM model_aliases WHERE alias = $1`, f.alias); err != nil {
		t.Fatalf("delete alias: %v", err)
	}
	f.clock.Advance(testCacheTTL + time.Second)
	wantStatus(t, f.chat(t, f.token), http.StatusNotFound, "deleted, past TTL")
}

func TestAdminChangeAppliesAtOnceOnTheSameInstance(t *testing.T) {
	admin := auth.Principal{Subject: "cache-admin", Roles: []string{auth.AdminRole}}
	f := newCacheFixture(t, &admin)
	f.srv.ensureUserFn = func(context.Context, auth.Principal) (ensuredUser, error) {
		return ensuredUser{roles: admin.Roles}, nil
	}
	f.srv.auditHook = func(context.Context, string, string, string, any) {}
	wantStatus(t, f.chat(t, f.token), http.StatusOK, "warm-up")

	req := httptest.NewRequest(http.MethodPost, "/api/admin/keys/"+f.keyID+"/revoke", nil)
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, req)
	if rec.Code/100 != 2 {
		t.Fatalf("admin revoke: status %d; body: %s", rec.Code, rec.Body.String())
	}
	wantStatus(t, f.chat(t, f.token), http.StatusUnauthorized, "revoked through this instance, no clock advance")
}

func TestReadyThroughABriefDatabaseOutage(t *testing.T) {
	f := newCacheFixture(t, nil)
	ready := func() int {
		rec := httptest.NewRecorder()
		f.srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		return rec.Code
	}
	if got := ready(); got != http.StatusOK {
		t.Fatalf("readyz with the database up = %d", got)
	}
	f.proxy.cut()
	if got := ready(); got != http.StatusOK {
		t.Errorf("readyz at the start of an outage = %d, want 200 — the pod can still serve from cache", got)
	}
	f.clock.Advance(testCacheMaxStale + time.Second)
	if got := ready(); got != http.StatusServiceUnavailable {
		t.Errorf("readyz after an outage longer than max stale = %d, want 503", got)
	}
	f.proxy.restore()
	if got := ready(); got != http.StatusOK {
		t.Errorf("readyz with the database back = %d, want 200", got)
	}
}

func TestNotReadyWhileDrainingButStillServing(t *testing.T) {
	f := newCacheFixture(t, nil)
	f.startLedger(t)
	f.srv.SetDraining()
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("readyz while draining = %d, want 503", rec.Code)
	}
	wantStatus(t, f.chat(t, f.token), http.StatusOK, "request while draining")
}

func TestFailedControlPlaneWriteKeepsTheCache(t *testing.T) {
	f := newCacheFixture(t, nil)
	wantStatus(t, f.chat(t, f.token), http.StatusOK, "warm-up")
	f.proxy.cut()

	// An unauthenticated write fails; it must not throw away what the
	// gateway is serving the outage from.
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/keys", strings.NewReader(`{}`)))
	if rec.Code/100 == 2 {
		t.Fatalf("unauthenticated POST /api/keys: status %d, want a failure", rec.Code)
	}
	wantStatus(t, f.chat(t, f.token), http.StatusOK, "database down, after a failed control-plane write")
}

func TestNotReadyWhenTheDatabaseWasNeverReached(t *testing.T) {
	f := newCacheFixture(t, nil)
	f.proxy.cut()
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("readyz on a pod that never reached Postgres = %d, want 503 — it has nothing cached to serve", rec.Code)
	}
}
