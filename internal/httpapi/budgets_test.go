package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
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

// budgetFixture is a database, a Redis, a mock provider behind an alias, and
// a fake clock on 3 Oct 2026 (UTC), from which gateway instances are built.
// Ledger rows the tests insert are dated inside that clock's periods.
type budgetFixture struct {
	pool     *pgxpool.Pool
	rdb      *redis.Client
	clock    *fakeClock
	alias    string
	provider string
	suffix   string
}

func newBudgetFixture(t *testing.T) *budgetFixture {
	t.Helper()
	rurl := os.Getenv("TEST_REDIS_URL")
	if rurl == "" {
		t.Skip("TEST_REDIS_URL not set; skipping budget test")
	}
	pool := testPool(t)
	ropt, err := redis.ParseURL(rurl)
	if err != nil {
		t.Fatalf("redis url: %v", err)
	}
	rdb := redis.NewClient(ropt)
	t.Cleanup(func() { _ = rdb.Close() })
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	f := &budgetFixture{
		pool: pool, rdb: rdb,
		clock:    &fakeClock{t: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)},
		alias:    "budget-alias-" + suffix,
		provider: "budget-mock-" + suffix,
		suffix:   suffix,
	}
	f.exec(t, `INSERT INTO providers (name, kind) VALUES ($1, 'mock')`, f.provider)
	f.exec(t, `INSERT INTO model_aliases (alias, protocol) VALUES ($1, 'openai')`, f.alias)
	f.exec(t, `INSERT INTO alias_targets (alias, priority, provider_name, upstream_model, upstream_protocol)
		VALUES ($1, 0, $2, 'mock-gpt', 'openai')`, f.alias, f.provider)
	// Users get a role allowing the alias, so the key snapshot rebuilt by an
	// admin user update still allows it.
	f.exec(t, `INSERT INTO roles_policy (role, allowed_models) VALUES ($1, $2)`, f.role(), []string{f.alias})
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, `DELETE FROM roles_policy WHERE role = $1`, f.role())
		_, _ = pool.Exec(bg, `DELETE FROM model_aliases WHERE alias = $1`, f.alias)
		_, _ = pool.Exec(bg, `DELETE FROM providers WHERE name = $1`, f.provider)
	})
	return f
}

func (f *budgetFixture) role() string { return "budget-role-" + f.suffix }

func (f *budgetFixture) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %s: %v", sql, err)
	}
}

// server builds one gateway instance (a replica) on the fixture.
func (f *budgetFixture) server(t *testing.T) *Server {
	t.Helper()
	reg := providers.NewRegistry()
	reg.Register(providers.NewMock(f.provider), 0)
	cfg := &config.Config{Env: "dev", LookupCacheTTL: time.Minute, LookupCacheMaxStale: time.Hour, LimitsTZ: time.UTC}
	s := NewServer(cfg, &store.Store{PG: f.pool, RDB: f.rdb}, Deps{
		Providers: reg,
		Limiter:   limits.New(f.rdb),
		Pricing:   pricing.New(),
		Auth:      noSession{},
		Now:       f.clock.Now,
	})
	s.auditHook = func(context.Context, string, string, string, any) {}
	s.Ledger().Start()
	t.Cleanup(s.Ledger().Stop)
	return s
}

// user inserts a user and removes it (and its ledger rows) afterwards.
func (f *budgetFixture) user(t *testing.T, name string) string {
	t.Helper()
	var id string
	if err := f.pool.QueryRow(context.Background(),
		`INSERT INTO users (subject, roles) VALUES ($1, $2) RETURNING id::text`, name+"-"+f.suffix, []string{f.role()}).Scan(&id); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = f.pool.Exec(bg, `DELETE FROM usage_ledger WHERE user_id = $1`, id)
		_, _ = f.pool.Exec(bg, `DELETE FROM api_keys WHERE user_id = $1`, id)
		_, _ = f.pool.Exec(bg, `DELETE FROM users WHERE id = $1`, id)
	})
	return id
}

// key mints an API key for userID with a policy snapshot allowing the alias
// and carrying limitsJSON (may be empty).
func (f *budgetFixture) key(t *testing.T, userID, limitsJSON string) (token, keyID string) {
	t.Helper()
	token = fmt.Sprintf("air_test_budget_%s_%d", f.suffix, time.Now().UnixNano())
	snap := fmt.Sprintf(`{"allowed_models":[%q]}`, f.alias)
	if limitsJSON != "" {
		snap = fmt.Sprintf(`{"allowed_models":[%q],"limits":%s}`, f.alias, limitsJSON)
	}
	if err := f.pool.QueryRow(context.Background(), `
		INSERT INTO api_keys (user_id, hash, prefix, last4, policy_snapshot)
		VALUES ($1, $2, 'air_test', 'test', $3) RETURNING id::text`,
		userID, apikey.Hash(token), snap).Scan(&keyID); err != nil {
		t.Fatalf("insert key: %v", err)
	}
	return token, keyID
}

// ledgerRow inserts a usage row dated ts; userID "" is a row with no user.
func (f *budgetFixture) ledgerRow(t *testing.T, userID string, ts time.Time, tokens int64, cost string) {
	t.Helper()
	var uid any
	if userID != "" {
		uid = userID
	}
	f.exec(t, `INSERT INTO usage_ledger (ts, user_id, prompt_tokens, cost_usd, status) VALUES ($1, $2, $3, $4::numeric, 200)`,
		ts, uid, tokens, cost)
	if userID == "" {
		t.Cleanup(func() {
			_, _ = f.pool.Exec(context.Background(), `DELETE FROM usage_ledger WHERE user_id IS NULL AND ts = $1`, ts)
		})
	}
}

func (f *budgetFixture) chat(t *testing.T, s *Server, token string) *httptest.ResponseRecorder {
	t.Helper()
	body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hello there"}]}`, f.alias)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

// as makes s treat every control-plane request as coming from subject,
// backed by user row userID, with roles.
func as(s *Server, subject, userID string, roles ...string) {
	p := auth.Principal{Subject: subject, UserID: userID, Roles: roles}
	s.auth = &fakeAuth{principal: p}
	s.ensureUserFn = func(context.Context, auth.Principal) (ensuredUser, error) {
		return ensuredUser{id: userID, roles: roles}, nil
	}
}

func asAdmin(s *Server) {
	as(s, "budget-admin", "00000000-0000-0000-0000-000000000000", auth.AdminRole)
}

func call(t *testing.T, s *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
}

func setBudgets(t *testing.T, f *budgetFixture, s *Server, userID, budgets string) {
	t.Helper()
	f.exec(t, `UPDATE users SET budgets = $2::jsonb WHERE id = $1`, userID, budgets)
	s.reloadUserInfo(context.Background())
}

// TestBudgetCounterRebuiltFromLedger: with no counter in Redis, the user's
// month so far is read from the ledger (rows outside the month ignored), the
// request is refused with the budget message, and the counter is created
// holding that sum. A later settled request increments it.
func TestBudgetCounterRebuiltFromLedger(t *testing.T) {
	f := newBudgetFixture(t)
	s := f.server(t)
	uid := f.user(t, "backfill")
	token, _ := f.key(t, uid, "")
	f.ledgerRow(t, uid, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), 100, "0.6")
	f.ledgerRow(t, uid, time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC), 50, "0.4")
	f.ledgerRow(t, uid, time.Date(2026, 9, 30, 23, 59, 0, 0, time.UTC), 1000, "5") // September
	setBudgets(t, f, s, uid, `{"cost_usd":{"month":1}}`)

	rec := f.chat(t, s, token)
	wantStatus(t, rec, http.StatusTooManyRequests, "month cost budget used up in the ledger")
	if want := "monthly cost budget $1.00 reached; resets 2026-11-01 00:00 UTC"; !strings.Contains(rec.Body.String(), want) {
		t.Errorf("body %s, want message %q", rec.Body.String(), want)
	}
	if !strings.Contains(rec.Body.String(), `"rate_limit_error"`) {
		t.Errorf("body %s, want the OpenAI rate_limit_error shape", rec.Body.String())
	}
	if v, err := f.rdb.Get(context.Background(), "air:ub:"+uid+":cost:m:2026-10").Int64(); err != nil || v != 1_000_000 {
		t.Errorf("cost counter = %d (%v), want 1000000 micro-USD", v, err)
	}

	// A token cap: rebuilt as 150 tokens, then a served request adds its own.
	setBudgets(t, f, s, uid, `{"tokens":{"month":1000000}}`)
	wantStatus(t, f.chat(t, s, token), http.StatusOK, "under the token cap")
	v, err := f.rdb.Get(context.Background(), "air:ub:"+uid+":tok:m:2026-10").Int64()
	if err != nil || v <= 150 {
		t.Errorf("token counter = %d (%v), want 150 from the ledger plus the served request", v, err)
	}
}

// TestBudgetDenialLeavesNoKeyReservation: the budget check runs before the
// per-key limiter, so a refused request never reserves key headroom.
func TestBudgetDenialLeavesNoKeyReservation(t *testing.T) {
	f := newBudgetFixture(t)
	s := f.server(t)
	uid := f.user(t, "noreserve")
	token, keyID := f.key(t, uid, `{"tokens":{"5h":100000}}`)
	t.Cleanup(func() { _ = f.rdb.Del(context.Background(), "air:u:"+keyID+":tok").Err() })
	f.ledgerRow(t, uid, time.Date(2026, 10, 3, 1, 0, 0, 0, time.UTC), 10, "0")
	setBudgets(t, f, s, uid, `{"tokens":{"day":10}}`)

	rec := f.chat(t, s, token)
	wantStatus(t, rec, http.StatusTooManyRequests, "daily token budget reached")
	if !strings.Contains(rec.Body.String(), "daily token budget 10 reached; resets 2026-10-04 00:00 UTC") {
		t.Errorf("body %s", rec.Body.String())
	}
	if n, err := f.rdb.HLen(context.Background(), "air:u:"+keyID+":tok").Result(); err != nil || n != 0 {
		t.Fatalf("key token buckets = %d (%v) after a budget 429, want none", n, err)
	}

	// The same key without the budget does go through the key limiter, which
	// is what the check above would have caught.
	setBudgets(t, f, s, uid, `{}`)
	wantStatus(t, f.chat(t, s, token), http.StatusOK, "budget removed")
	if n, _ := f.rdb.HLen(context.Background(), "air:u:"+keyID+":tok").Result(); n == 0 {
		t.Error("the key limiter wrote nothing for a served request; the check above proves nothing")
	}
}

// TestBudgetEditReachesAnotherReplica: a budget saved through one instance
// applies there at once, and on another instance after its config refresh.
func TestBudgetEditReachesAnotherReplica(t *testing.T) {
	f := newBudgetFixture(t)
	s1, s2 := f.server(t), f.server(t)
	asAdmin(s1)
	uid := f.user(t, "replica")
	token, _ := f.key(t, uid, "")
	f.ledgerRow(t, uid, time.Date(2026, 10, 3, 1, 0, 0, 0, time.UTC), 10, "0")
	wantStatus(t, f.chat(t, s2, token), http.StatusOK, "no budget yet")

	rec := call(t, s1, http.MethodPut, "/api/admin/users/"+uid, `{"budgets":{"tokens":{"day":5}}}`)
	wantStatus(t, rec, http.StatusOK, "admin sets a budget")
	wantStatus(t, f.chat(t, s1, token), http.StatusTooManyRequests, "the instance that saved it")
	wantStatus(t, f.chat(t, s2, token), http.StatusOK, "another instance before its refresh")
	s2.RefreshConfig(context.Background())
	wantStatus(t, f.chat(t, s2, token), http.StatusTooManyRequests, "another instance after its refresh")

	wantStatus(t, call(t, s1, http.MethodPut, "/api/admin/users/"+uid, `{"budgets":{}}`), http.StatusOK, "admin clears it")
	s2.RefreshConfig(context.Background())
	wantStatus(t, f.chat(t, s2, token), http.StatusOK, "cleared, after refresh")
}

func TestUserUpdateValidatesBudgetsAndGroup(t *testing.T) {
	f := newBudgetFixture(t)
	s := f.server(t)
	asAdmin(s)
	uid := f.user(t, "validate")
	wantStatus(t, call(t, s, http.MethodPut, "/api/admin/users/"+uid,
		`{"budgets":{"tokens":{"week":2000000},"cost_usd":{"day":5,"month":50}}}`), http.StatusOK, "valid budgets")
	for _, body := range []string{
		`{"budgets":{"tokens":{"day":-1}}}`,
		`{"budgets":{"cost_usd":{"month":-5}}}`,
		`{"budgets":{"tokens":{"year":1}}}`,
		`{"budgets":{"requests":{"day":1}}}`,
		`{"budgets":{"tokens":{"day":0.5}}}`,
		`{"budgets":"lots"}`,
		`{"group_id":"not-a-uuid"}`,
		`{"group_id":42}`,
		`{"group_id":"7c3b9f8e-0000-4000-8000-000000000000"}`, // no such group
	} {
		rec := call(t, s, http.MethodPut, "/api/admin/users/"+uid, body)
		wantStatus(t, rec, http.StatusBadRequest, body)
	}
	// Fields left out keep what is stored.
	wantStatus(t, call(t, s, http.MethodPut, "/api/admin/users/"+uid, `{"display":"V"}`), http.StatusOK, "display only")
	var raw string
	if err := f.pool.QueryRow(context.Background(), `SELECT budgets::text FROM users WHERE id = $1`, uid).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var stored map[string]map[string]float64
	_ = json.Unmarshal([]byte(raw), &stored)
	if stored["tokens"]["week"] != 2000000 || stored["cost_usd"]["day"] != 5 || stored["cost_usd"]["month"] != 50 {
		t.Errorf("stored budgets %s, want the valid save kept", raw)
	}
}

// adminUser is the part of a GET /api/admin/users row these tests read.
type adminUser struct {
	ID        string  `json:"id"`
	GroupID   *string `json:"group_id"`
	GroupName string  `json:"group_name"`
	Budgets   struct {
		Tokens  map[string]int64   `json:"tokens"`
		CostUSD map[string]float64 `json:"cost_usd"`
	} `json:"budgets"`
	Spend map[string]struct {
		Tokens  int64   `json:"tokens"`
		CostUSD float64 `json:"cost_usd"`
	} `json:"spend"`
}

func listedUser(t *testing.T, s *Server, id string) adminUser {
	t.Helper()
	rec := call(t, s, http.MethodGet, "/api/admin/users", "")
	wantStatus(t, rec, http.StatusOK, "list users")
	var out struct {
		Users []adminUser `json:"users"`
	}
	decodeBody(t, rec, &out)
	for _, u := range out.Users {
		if u.ID == id {
			return u
		}
	}
	t.Fatalf("user %s not listed", id)
	return adminUser{}
}

func TestGroupsCRUDAndDeleteUngroupsMembers(t *testing.T) {
	f := newBudgetFixture(t)
	s := f.server(t)
	asAdmin(s)
	uid := f.user(t, "member")
	name := "developers-" + f.suffix

	rec := call(t, s, http.MethodPost, "/api/admin/groups", fmt.Sprintf(`{"name":%q,"description":"devs"}`, name))
	wantStatus(t, rec, http.StatusCreated, "create")
	var g groupView
	decodeBody(t, rec, &g)
	t.Cleanup(func() { _, _ = f.pool.Exec(context.Background(), `DELETE FROM user_groups WHERE id = $1`, g.ID) })
	if g.ID == "" || g.Name != name || g.Description != "devs" {
		t.Fatalf("created %+v", g)
	}
	wantStatus(t, call(t, s, http.MethodPost, "/api/admin/groups", fmt.Sprintf(`{"name":%q}`, name)), http.StatusConflict, "duplicate name")
	wantStatus(t, call(t, s, http.MethodPost, "/api/admin/groups", `{"name":"  "}`), http.StatusBadRequest, "blank name")

	wantStatus(t, call(t, s, http.MethodPut, "/api/admin/users/"+uid, fmt.Sprintf(`{"group_id":%q}`, g.ID)), http.StatusOK, "join")
	if u := listedUser(t, s, uid); u.GroupID == nil || *u.GroupID != g.ID || u.GroupName != name {
		t.Errorf("listed user group = %v %q, want %s %q", u.GroupID, u.GroupName, g.ID, name)
	}

	renamed := name + "-x"
	rec = call(t, s, http.MethodPut, "/api/admin/groups/"+g.ID, fmt.Sprintf(`{"name":%q}`, renamed))
	wantStatus(t, rec, http.StatusOK, "rename")
	var upd groupView
	decodeBody(t, rec, &upd)
	if upd.Name != renamed || upd.Description != "devs" || upd.MemberCount != 1 {
		t.Errorf("updated %+v, want renamed, description kept, 1 member", upd)
	}
	if got := s.userInfoFor(uid).GroupName; got != renamed {
		t.Errorf("cached group name %q, want %q", got, renamed)
	}

	rec = call(t, s, http.MethodGet, "/api/admin/groups", "")
	wantStatus(t, rec, http.StatusOK, "list")
	var list struct {
		Groups []groupView `json:"groups"`
	}
	decodeBody(t, rec, &list)
	found := false
	for _, x := range list.Groups {
		if x.ID == g.ID {
			found = x.MemberCount == 1 && x.Name == renamed
		}
	}
	if !found {
		t.Errorf("groups %+v, want %s with one member", list.Groups, g.ID)
	}

	wantStatus(t, call(t, s, http.MethodDelete, "/api/admin/groups/"+g.ID, ""), http.StatusOK, "delete")
	if u := listedUser(t, s, uid); u.GroupID != nil || u.GroupName != "" {
		t.Errorf("after delete user group = %v %q, want none", u.GroupID, u.GroupName)
	}
	if got := s.userInfoFor(uid).GroupName; got != "" {
		t.Errorf("cached group name %q after delete, want none", got)
	}
	wantStatus(t, call(t, s, http.MethodDelete, "/api/admin/groups/"+g.ID, ""), http.StatusNotFound, "delete again")
	wantStatus(t, call(t, s, http.MethodPut, "/api/admin/groups/"+g.ID, `{"name":"x"}`), http.StatusNotFound, "update deleted")
	wantStatus(t, call(t, s, http.MethodDelete, "/api/admin/groups/nope", ""), http.StatusNotFound, "malformed id")
}

func TestUsersListCarriesBudgetsAndSpend(t *testing.T) {
	f := newBudgetFixture(t)
	s := f.server(t)
	asAdmin(s)
	uid := f.user(t, "spend")
	f.ledgerRow(t, uid, time.Date(2026, 10, 3, 1, 0, 0, 0, time.UTC), 10, "1.25")  // today (Saturday)
	f.ledgerRow(t, uid, time.Date(2026, 9, 29, 1, 0, 0, 0, time.UTC), 20, "2")     // this week, last month
	f.ledgerRow(t, uid, time.Date(2026, 10, 1, 1, 0, 0, 0, time.UTC), 40, "4")     // this week and month
	f.ledgerRow(t, uid, time.Date(2026, 9, 20, 1, 0, 0, 0, time.UTC), 1000, "100") // neither
	setBudgets(t, f, s, uid, `{"cost_usd":{"month":50}}`)

	u := listedUser(t, s, uid)
	if u.Budgets.CostUSD["month"] != 50 || u.Budgets.Tokens["day"] != 0 {
		t.Errorf("budgets %+v", u.Budgets)
	}
	want := map[string][2]float64{"day": {10, 1.25}, "week": {70, 7.25}, "month": {50, 5.25}}
	for p, w := range want {
		got := u.Spend[p]
		if float64(got.Tokens) != w[0] || got.CostUSD != w[1] {
			t.Errorf("spend[%s] = %+v, want tokens %v cost %v", p, got, w[0], w[1])
		}
	}
}

func TestUsageByGroupReport(t *testing.T) {
	f := newBudgetFixture(t)
	s := f.server(t)
	asAdmin(s)
	alice, bob := f.user(t, "alice"), f.user(t, "bob")
	rec := call(t, s, http.MethodPost, "/api/admin/groups", fmt.Sprintf(`{"name":"finance-%s"}`, f.suffix))
	wantStatus(t, rec, http.StatusCreated, "create group")
	var g groupView
	decodeBody(t, rec, &g)
	t.Cleanup(func() { _, _ = f.pool.Exec(context.Background(), `DELETE FROM user_groups WHERE id = $1`, g.ID) })
	wantStatus(t, call(t, s, http.MethodPut, "/api/admin/users/"+alice, fmt.Sprintf(`{"group_id":%q}`, g.ID)), http.StatusOK, "alice joins")

	day := time.Date(2026, 10, 3, 2, 0, 0, 0, time.UTC)
	f.ledgerRow(t, alice, day, 100, "3")
	f.ledgerRow(t, alice, day.Add(time.Minute), 50, "1")
	f.ledgerRow(t, bob, day, 10, "0.5")
	f.ledgerRow(t, "", day.Add(time.Second*7), 5, "0.25")                          // a deleted user's row
	f.ledgerRow(t, alice, time.Date(2026, 9, 30, 2, 0, 0, 0, time.UTC), 999, "99") // last month

	rec = call(t, s, http.MethodGet, "/api/admin/usage/groups?period=month", "")
	wantStatus(t, rec, http.StatusOK, "report")
	var rep struct {
		Period string      `json:"period"`
		TZ     string      `json:"tz"`
		Start  time.Time   `json:"start"`
		End    time.Time   `json:"end"`
		Total  usageTotals `json:"total"`
		Groups []struct {
			GroupID  *string `json:"group_id"`
			Name     string  `json:"name"`
			Tokens   int64   `json:"tokens"`
			CostUSD  float64 `json:"cost_usd"`
			Requests int64   `json:"requests"`
			Users    []struct {
				UserID   *string `json:"user_id"`
				Subject  string  `json:"subject"`
				Tokens   int64   `json:"tokens"`
				CostUSD  float64 `json:"cost_usd"`
				Requests int64   `json:"requests"`
			} `json:"users"`
		} `json:"groups"`
	}
	decodeBody(t, rec, &rep)
	if rep.Period != "month" || rep.TZ != "UTC" || !rep.Start.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)) || !rep.End.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("header %s %s [%v, %v)", rep.Period, rep.TZ, rep.Start, rep.End)
	}

	// The org total over the same period, straight from the ledger.
	var orgTokens, orgCostMicro, orgRequests int64
	if err := f.pool.QueryRow(context.Background(), `
		SELECT COALESCE(SUM(prompt_tokens + completion_tokens), 0)::bigint,
		       COALESCE(round(SUM(cost_usd) * 1000000), 0)::bigint, count(*)
		FROM usage_ledger WHERE ts >= $1 AND ts < $2`, rep.Start, rep.End).Scan(&orgTokens, &orgCostMicro, &orgRequests); err != nil {
		t.Fatal(err)
	}
	if rep.Total.Tokens != orgTokens || rep.Total.Requests != orgRequests || int64(rep.Total.CostUSD*1e6+0.5) != orgCostMicro {
		t.Errorf("total %+v, want the org total %d tokens, %d micro-USD, %d requests", rep.Total, orgTokens, orgCostMicro, orgRequests)
	}
	var sumTokens, sumRequests int64
	var sumCostMicro int64
	var ungrouped, finance = -1, -1
	for i, grp := range rep.Groups {
		sumTokens += grp.Tokens
		sumRequests += grp.Requests
		sumCostMicro += int64(grp.CostUSD*1e6 + 0.5)
		if grp.GroupID == nil {
			ungrouped = i
		} else if *grp.GroupID == g.ID {
			finance = i
		}
		if i > 0 && grp.CostUSD > rep.Groups[i-1].CostUSD {
			t.Errorf("groups not sorted by cost: %q after %q", grp.Name, rep.Groups[i-1].Name)
		}
	}
	if sumTokens != rep.Total.Tokens || sumRequests != rep.Total.Requests || sumCostMicro != orgCostMicro {
		t.Errorf("groups add up to %d tokens / %d micro-USD / %d requests, total is %+v", sumTokens, sumCostMicro, sumRequests, rep.Total)
	}
	if ungrouped < 0 || rep.Groups[ungrouped].Name != "Ungrouped" {
		t.Fatalf("no Ungrouped entry in %+v", rep.Groups)
	}
	if finance < 0 {
		t.Fatalf("group %s missing", g.ID)
	}
	fg := rep.Groups[finance]
	if fg.Tokens != 150 || fg.CostUSD != 4 || fg.Requests != 2 || len(fg.Users) != 1 || fg.Users[0].UserID == nil || *fg.Users[0].UserID != alice {
		t.Errorf("finance = %+v, want alice's 150 tokens, $4, 2 requests", fg)
	}
	var bobSeen, deletedSeen bool
	for _, u := range rep.Groups[ungrouped].Users {
		if u.UserID != nil && *u.UserID == bob && u.Tokens == 10 && u.CostUSD == 0.5 {
			bobSeen = true
		}
		if u.UserID == nil {
			deletedSeen = true
		}
	}
	if !bobSeen || !deletedSeen {
		t.Errorf("ungrouped users %+v, want bob and the deleted user's row", rep.Groups[ungrouped].Users)
	}

	wantStatus(t, call(t, s, http.MethodGet, "/api/admin/usage/groups?period=year", ""), http.StatusBadRequest, "bad period")
	rec = call(t, s, http.MethodGet, "/api/admin/usage/groups?period=day", "")
	wantStatus(t, rec, http.StatusOK, "day report")
	decodeBody(t, rec, &rep)
	if !rep.Start.Equal(day.Truncate(24*time.Hour)) || rep.Period != "day" {
		t.Errorf("day report starts %v", rep.Start)
	}
}

func TestMeCarriesBudgetAndGroup(t *testing.T) {
	f := newBudgetFixture(t)
	s := f.server(t)
	uid := f.user(t, "me")
	var gid string
	if err := f.pool.QueryRow(context.Background(), `INSERT INTO user_groups (name) VALUES ($1) RETURNING id::text`, "me-group-"+f.suffix).Scan(&gid); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = f.pool.Exec(context.Background(), `DELETE FROM user_groups WHERE id = $1`, gid) })
	f.exec(t, `UPDATE users SET group_id = $2, budgets = '{"tokens":{"day":1000},"cost_usd":{"month":50}}' WHERE id = $1`, uid, gid)
	f.ledgerRow(t, uid, time.Date(2026, 10, 3, 1, 0, 0, 0, time.UTC), 300, "12.4")
	as(s, "me-"+f.suffix, uid)

	rec := call(t, s, http.MethodGet, "/api/me", "")
	wantStatus(t, rec, http.StatusOK, "me")
	var me struct {
		Subject   string `json:"subject"`
		GroupName string `json:"group_name"`
		Budget    struct {
			TZ      string `json:"tz"`
			Periods map[string]struct {
				Start        time.Time `json:"start"`
				End          time.Time `json:"end"`
				TokensUsed   int64     `json:"tokens_used"`
				TokensLimit  int64     `json:"tokens_limit"`
				CostUSDUsed  float64   `json:"cost_usd_used"`
				CostUSDLimit float64   `json:"cost_usd_limit"`
			} `json:"periods"`
		} `json:"budget"`
	}
	decodeBody(t, rec, &me)
	if me.GroupName != "me-group-"+f.suffix || me.Budget.TZ != "UTC" || len(me.Budget.Periods) != 3 {
		t.Fatalf("me = %s", rec.Body.String())
	}
	d, m := me.Budget.Periods["day"], me.Budget.Periods["month"]
	if d.TokensUsed != 300 || d.TokensLimit != 1000 || d.CostUSDLimit != 0 || !d.Start.Equal(time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)) || !d.End.Equal(time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("day = %+v", d)
	}
	if m.CostUSDUsed != 12.4 || m.CostUSDLimit != 50 || m.TokensLimit != 0 {
		t.Errorf("month = %+v", m)
	}
	if w := me.Budget.Periods["week"]; !w.Start.Equal(time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("week starts %v, want Monday 28 Sep", w.Start)
	}
}

// TestLiveEntryCarriesGroup: an in-flight request is labelled with its
// owner's current group from the in-memory user snapshot.
func TestLiveEntryCarriesGroup(t *testing.T) {
	s := &Server{live: newLiveRegistry("pod-a")}
	s.userInfo.Store(&map[string]userInfo{"u-dev": {GroupName: "developers"}})
	run := func(userID string) liveRequest {
		var seen []liveRequest
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		r = r.WithContext(context.WithValue(r.Context(), keyCtxKey, authedKey{KeyID: "k", UserID: userID}))
		s.trackLive("chat", func(http.ResponseWriter, *http.Request) {
			seen = getLive(t, s).Requests
		})(httptest.NewRecorder(), r)
		if len(seen) != 1 {
			t.Fatalf("in flight: %+v", seen)
		}
		return seen[0]
	}
	if got := run("u-dev").Group; got != "developers" {
		t.Errorf("group %q, want developers", got)
	}
	if got := run("u-other").Group; got != "" {
		t.Errorf("ungrouped user's group %q, want empty", got)
	}
	// A pod that predates groups publishes entries without the field.
	var old podSnapshot
	if err := json.Unmarshal([]byte(`{"pod":"old","requests":[{"id":"x","user":"u"}]}`), &old); err != nil || old.Requests[0].Group != "" {
		t.Errorf("old snapshot: %+v %v", old, err)
	}
}
