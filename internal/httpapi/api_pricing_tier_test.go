package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/auth"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/pricing"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/providers"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

// pricingServer builds the bare admin server the pricing endpoints need: a
// real pool, an admin session, and an empty in-memory price table. Same shape
// as TestAdminPricingImport's, which is where the pattern comes from.
func pricingServer(t *testing.T, pool *pgxpool.Pool) *Server {
	t.Helper()
	s := &Server{
		mux:  http.NewServeMux(),
		st:   &store.Store{PG: pool},
		auth: &fakeAuth{principal: auth.Principal{Subject: "admin", Roles: []string{auth.AdminRole}}},
		ensureUserFn: func(_ context.Context, p auth.Principal) (string, error) {
			return "uid-" + p.Subject, nil
		},
		pricing: pricing.New(),
	}
	reg := providers.NewRegistry()
	reg.Register(providers.NewMock("mock"), 0)
	s.regPtr.Store(reg)
	s.mux.HandleFunc("PUT /api/admin/pricing/{model}", s.requireAdmin(s.handleAdminPutPricing))
	s.mux.HandleFunc("GET /api/admin/pricing", s.requireAdmin(s.handleAdminPricing))
	s.mux.HandleFunc("POST /api/admin/pricing/import/{provider}", s.requireAdmin(s.handleAdminPricingImport))
	return s
}

func putPrice(t *testing.T, s *Server, model, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/api/admin/pricing/"+model, strings.NewReader(body))
	rw := httptest.NewRecorder()
	s.mux.ServeHTTP(rw, req)
	return rw
}

// TestAdminPutPricingContextTier saves Gemini 2.5 Pro's real published shape
// through the endpoint and asserts the threshold survives the round trip into
// both the table and the in-memory snapshot cost is read from. The dev
// Postgres is shared, so the fixture row is deleted in t.Cleanup.
func TestAdminPutPricingContextTier(t *testing.T) {
	pool := testPool(t)
	s := pricingServer(t, pool)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM pricing WHERE provider = 'tiertest'`)
	})

	rw := putPrice(t, s, "tier-pro", `{"provider":"tiertest","unit":"tokens",
		"input_per_1m":1.25,"output_per_1m":10,
		"context_threshold":200000,"input_per_1m_above":2.5,"output_per_1m_above":15}`)
	if rw.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s, want 200", rw.Code, rw.Body.String())
	}

	var threshold int
	var inAbove, outAbove float64
	if err := pool.QueryRow(context.Background(),
		`SELECT context_threshold, input_per_1m_above, output_per_1m_above
		 FROM pricing WHERE provider = 'tiertest' AND model = 'tier-pro'`).
		Scan(&threshold, &inAbove, &outAbove); err != nil {
		t.Fatalf("row not found after save: %v", err)
	}
	if threshold != 200_000 || inAbove != 2.5 || outAbove != 15 {
		t.Errorf("stored tier = (%d, %v, %v), want (200000, 2.5, 15)", threshold, inAbove, outAbove)
	}

	// The saved row prices a long prompt at the high rates immediately, without
	// waiting for a reload: 300000/1e6*2.50 + 1000/1e6*15.
	if got := s.pricing.CostMicroUSD("tiertest", "tier-pro", 300_000, 1000); got != 765_000 {
		t.Errorf("in-memory long-prompt cost = %d, want 765000", got)
	}
	// And a short one exactly as before: 1000/1e6*1.25 + 1000/1e6*10.
	if got := s.pricing.CostMicroUSD("tiertest", "tier-pro", 1000, 1000); got != 11_250 {
		t.Errorf("in-memory short-prompt cost = %d, want 11250", got)
	}

	// The list endpoint reports the tier, so the console can show it.
	req := httptest.NewRequest(http.MethodGet, "/api/admin/pricing", nil)
	lw := httptest.NewRecorder()
	s.mux.ServeHTTP(lw, req)
	if !strings.Contains(lw.Body.String(), `"context_threshold":200000`) {
		t.Errorf("list response omits the threshold: %s", lw.Body.String())
	}
}

// TestAdminPutPricingRejectsIncompleteTier: a threshold with no rate above it
// would price a long prompt at nothing — worse than the flat rate it replaced —
// so it is refused at save time rather than discovered on a bill.
func TestAdminPutPricingRejectsIncompleteTier(t *testing.T) {
	pool := testPool(t)
	s := pricingServer(t, pool)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM pricing WHERE provider = 'tiertest'`)
	})

	rw := putPrice(t, s, "tier-broken", `{"provider":"tiertest","unit":"tokens",
		"input_per_1m":1.25,"output_per_1m":10,"context_threshold":200000}`)
	if rw.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, body = %s, want 400", rw.Code, rw.Body.String())
	}

	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM pricing WHERE provider = 'tiertest' AND model = 'tier-broken'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("rejected row was stored anyway (%d rows)", n)
	}
}

// TestAdminPricingImportKeepsHandEnteredTier: a catalog import carries flat
// rates and knows nothing of tiers. It must not silently drop one an operator
// entered by hand — including from the in-memory table, which is what cost is
// actually read from between reloads.
func TestAdminPricingImportKeepsHandEnteredTier(t *testing.T) {
	pool := testPool(t)
	s := pricingServer(t, pool)
	ctx := context.Background()
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM pricing WHERE provider = 'mock' AND model = 'mock-gpt'`)
	})

	rw := putPrice(t, s, "mock-gpt", `{"provider":"mock","unit":"tokens",
		"input_per_1m":1,"output_per_1m":2,
		"context_threshold":1000,"input_per_1m_above":10,"output_per_1m_above":20}`)
	if rw.Code != http.StatusOK {
		t.Fatalf("seed save: code = %d, body = %s", rw.Code, rw.Body.String())
	}

	req := httptest.NewRequest(http.MethodPost, "/api/admin/pricing/import/mock", nil)
	iw := httptest.NewRecorder()
	s.mux.ServeHTTP(iw, req)
	if iw.Code != http.StatusOK {
		t.Fatalf("import: code = %d, body = %s", iw.Code, iw.Body.String())
	}

	var threshold int
	var inAbove, outAbove float64
	if err := pool.QueryRow(ctx,
		`SELECT context_threshold, input_per_1m_above, output_per_1m_above
		 FROM pricing WHERE provider = 'mock' AND model = 'mock-gpt'`).
		Scan(&threshold, &inAbove, &outAbove); err != nil {
		t.Fatalf("row not found after import: %v", err)
	}
	if threshold != 1000 || inAbove != 10 || outAbove != 20 {
		t.Errorf("import erased the stored tier: (%d, %v, %v)", threshold, inAbove, outAbove)
	}
	// A 2000-token prompt is over the 1000 breakpoint, so it prices at the
	// above rate the catalog never mentioned: 2000/1e6*10 = $0.02. The tier
	// still applies in memory, not just on disk.
	if got := s.pricing.CostMicroUSD("mock", "mock-gpt", 2000, 0); got != 20_000 {
		t.Errorf("in-memory tier lost after import: cost = %d, want 20000", got)
	}
}

// TestPricingContextTierConstraint pins the invariant at the layer that has to
// hold it: these Vertex price rows are hand-entered, and a hand reaches for
// SQL as readily as for the console. Validating only in the admin handler
// would let a direct INSERT store a threshold with no rate above it, which
// loads happily and bills a long prompt at $0.
func TestPricingContextTierConstraint(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		row  string
	}{
		{"threshold with no rates above it",
			`('con', 'a', 1, 2, 'tokens', 200000, 0, 0)`},
		{"threshold with only an input rate above it",
			`('con', 'b', 1, 2, 'tokens', 200000, 2.5, 0)`},
		{"threshold on a row priced by audio duration",
			`('con', 'c', 1, 0, 'audio_second', 200000, 2.5, 15)`},
		{"negative threshold",
			`('con', 'd', 1, 2, 'tokens', -1, 2.5, 15)`},
	} {
		// Each in its own transaction, rolled back: a constraint violation
		// aborts the transaction it happens in.
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("%s: begin: %v", tc.name, err)
		}
		_, err = tx.Exec(ctx, `INSERT INTO pricing
			(provider, model, input_per_1m, output_per_1m, unit,
			 context_threshold, input_per_1m_above, output_per_1m_above)
			VALUES `+tc.row)
		if err == nil {
			t.Errorf("%s: was accepted, want a constraint violation", tc.name)
		}
		_ = tx.Rollback(ctx)
	}

	// The shapes that are meant to work still do: a complete tier, and an
	// untiered row of any unit.
	for _, tc := range []struct {
		name string
		row  string
	}{
		{"complete tier", `('con', 'e', 1.25, 10, 'tokens', 200000, 2.5, 15)`},
		{"untiered token row", `('con', 'f', 1.25, 10, 'tokens', 0, 0, 0)`},
		{"untiered audio row", `('con', 'g', 100, 0, 'audio_second', 0, 0, 0)`},
	} {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("%s: begin: %v", tc.name, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO pricing
			(provider, model, input_per_1m, output_per_1m, unit,
			 context_threshold, input_per_1m_above, output_per_1m_above)
			VALUES `+tc.row); err != nil {
			t.Errorf("%s: was rejected: %v", tc.name, err)
		}
		_ = tx.Rollback(ctx)
	}
}
