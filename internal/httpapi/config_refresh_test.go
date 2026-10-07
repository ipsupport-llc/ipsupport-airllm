package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Seam tests for the periodic config refresh. A second replica's admin API is
// played by writes through the fixture's direct pool: this instance only learns
// about them by refreshing, exactly as it would behind a load balancer.

func TestProviderAddedOnAnotherReplicaIsServedAfterRefresh(t *testing.T) {
	f := newCacheFixture(t, nil)
	ctx := context.Background()
	wantStatus(t, f.chat(t, f.token), http.StatusOK, "warm-up")

	moved := fmt.Sprintf("refresh-mock-%d", time.Now().UnixNano())
	if _, err := f.direct.Exec(ctx, `INSERT INTO providers (name, kind) VALUES ($1, 'mock')`, moved); err != nil {
		t.Fatalf("insert provider: %v", err)
	}
	t.Cleanup(func() {
		_, _ = f.direct.Exec(context.Background(), `DELETE FROM alias_targets WHERE provider_name = $1`, moved)
		_, _ = f.direct.Exec(context.Background(), `DELETE FROM providers WHERE name = $1`, moved)
	})
	if _, err := f.direct.Exec(ctx, `UPDATE alias_targets SET provider_name = $2 WHERE alias = $1`, f.alias, moved); err != nil {
		t.Fatalf("retarget alias: %v", err)
	}
	f.clock.Advance(testCacheTTL + time.Second) // the alias edit itself is picked up by the lookup cache
	wantStatus(t, f.chat(t, f.token), http.StatusBadGateway, "provider unknown here before a refresh")

	f.srv.RefreshConfig(ctx)
	wantStatus(t, f.chat(t, f.token), http.StatusOK, "after refresh")
}

func TestSettingChangedOnAnotherReplicaAppliesAfterRefresh(t *testing.T) {
	f := newCacheFixture(t, nil)
	ctx := context.Background()
	restoreSetting(t, f, "failover")

	putSetting(t, f, "failover", `{"timeout_ms": 1234}`)
	if got := f.srv.failoverCfg().TimeoutMS; got == 1234 {
		t.Fatalf("failover timeout applied before any refresh")
	}
	f.srv.RefreshConfig(ctx)
	if got := f.srv.failoverCfg().TimeoutMS; got != 1234 {
		t.Errorf("failover timeout after refresh = %d, want 1234", got)
	}
}

func TestRefreshKeepsTheLastConfigWhileTheDatabaseIsDown(t *testing.T) {
	f := newCacheFixture(t, nil)
	ctx := context.Background()
	restoreSetting(t, f, "failover")
	putSetting(t, f, "failover", `{"timeout_ms": 1234}`)
	f.srv.RefreshConfig(ctx)
	wantStatus(t, f.chat(t, f.token), http.StatusOK, "warm-up")

	f.proxy.cut()
	f.srv.RefreshConfig(ctx)
	if got := f.srv.failoverCfg().TimeoutMS; got != 1234 {
		t.Errorf("failover timeout after a refresh during an outage = %d, want 1234 kept", got)
	}
	wantStatus(t, f.chat(t, f.token), http.StatusOK, "providers kept through the outage")
}

func TestFailedReadAfterASaveKeepsTheCurrentSetting(t *testing.T) {
	f := newCacheFixture(t, nil)
	ctx := context.Background()
	restoreSetting(t, f, "failover")
	putSetting(t, f, "failover", `{"timeout_ms": 1234}`)
	f.srv.loadFailover(ctx)

	f.proxy.cut() // the save's own write went through; the read-back does not
	f.srv.loadFailover(ctx)
	if got := f.srv.failoverCfg().TimeoutMS; got != 1234 {
		t.Errorf("failover timeout after a failed read-back = %d, want 1234 kept, not the defaults", got)
	}
}

func TestRefreshLeavesAnUnchangedRegistryInPlace(t *testing.T) {
	f := newCacheFixture(t, nil)
	ctx := context.Background()
	f.srv.RefreshConfig(ctx) // the first refresh may adopt the database's view
	before := f.srv.reg()
	f.srv.RefreshConfig(ctx)
	if f.srv.reg() != before {
		t.Error("a refresh with no provider change rebuilt the registry, resetting every concurrency slot")
	}
}

func TestLocalProviderSaveDoesNotTriggerASecondRebuild(t *testing.T) {
	f := newCacheFixture(t, nil)
	ctx := context.Background()
	if err := f.srv.ReloadProviders(ctx); err != nil { // what a provider save on this replica does
		t.Fatalf("reload: %v", err)
	}
	before := f.srv.reg()
	f.srv.RefreshConfig(ctx)
	if f.srv.reg() != before {
		t.Error("the refresh after a local provider save rebuilt the registry again")
	}
}

func TestPriceSavedOnAnotherReplicaCostsAfterRefresh(t *testing.T) {
	f := newCacheFixture(t, nil)
	other := pricingServer(t, f.direct) // the replica whose admin API takes the PUT and the import
	ctx := context.Background()
	provider := fmt.Sprintf("refresh-price-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = f.direct.Exec(bg, `DELETE FROM pricing WHERE provider = $1`, provider)
		_, _ = f.direct.Exec(bg, `DELETE FROM pricing WHERE provider = 'mock' AND model = 'mock-gpt'`)
	})
	cost := func() int64 { return f.srv.pricing.AudioCostMicroUSD(provider, "stt", 1_000_000) }
	imported := func() int64 { return f.srv.pricing.CostMicroUSD("mock", "mock-gpt", 1_000_000, 1_000_000) }
	if _, err := f.direct.Exec(ctx, `DELETE FROM pricing WHERE provider = 'mock' AND model = 'mock-gpt'`); err != nil {
		t.Fatalf("clear the import's row: %v", err)
	}

	f.srv.RefreshConfig(ctx)
	wantStatus(t, putPrice(t, other, "stt", fmt.Sprintf(`{"provider":%q,"unit":"audio_second","input_per_1m":1}`, provider)), http.StatusOK, "new price")
	if got := cost(); got != 0 {
		t.Fatalf("new price applied before any refresh: %d", got)
	}
	f.srv.RefreshConfig(ctx)
	if got := cost(); got != 1_000_000 {
		t.Errorf("new price after refresh = %d, want 1000000", got)
	}

	wantStatus(t, putPrice(t, other, "stt", fmt.Sprintf(`{"provider":%q,"unit":"audio_second","input_per_1m":3}`, provider)), http.StatusOK, "changed price")
	if got := cost(); got != 1_000_000 {
		t.Fatalf("changed price applied before any refresh: %d", got)
	}
	f.srv.RefreshConfig(ctx)
	if got := cost(); got != 3_000_000 {
		t.Errorf("changed price after refresh = %d, want 3000000", got)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/admin/pricing/import/mock", nil)
	rw := httptest.NewRecorder()
	other.mux.ServeHTTP(rw, req)
	wantStatus(t, rw, http.StatusOK, "import")
	if got := imported(); got != 0 {
		t.Fatalf("imported price applied before any refresh: %d", got)
	}
	f.srv.RefreshConfig(ctx)
	if got := imported(); got != 3_000_000 {
		t.Errorf("imported price after refresh = %d, want 3000000", got)
	}
}

func TestRefreshKeepsThePricesWhileTheDatabaseIsDown(t *testing.T) {
	f := newCacheFixture(t, nil)
	ctx := context.Background()
	provider := fmt.Sprintf("refresh-price-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = f.direct.Exec(context.Background(), `DELETE FROM pricing WHERE provider = $1`, provider)
	})
	if _, err := f.direct.Exec(ctx, `INSERT INTO pricing (provider, model, input_per_1m, output_per_1m) VALUES ($1, 'm', 1, 2)`, provider); err != nil {
		t.Fatalf("insert price: %v", err)
	}
	f.srv.RefreshConfig(ctx)

	f.proxy.cut()
	f.srv.RefreshConfig(ctx)
	if got := f.srv.pricing.CostMicroUSD(provider, "m", 1_000_000, 1_000_000); got != 3_000_000 {
		t.Errorf("price after a refresh during an outage = %d, want 3000000 kept", got)
	}
}

// putSetting writes a settings row the way another replica's admin API would.
func putSetting(t *testing.T, f *cacheFixture, name, value string) {
	t.Helper()
	if _, err := f.direct.Exec(context.Background(), `
		INSERT INTO settings (name, value) VALUES ($1, $2::jsonb)
		ON CONFLICT (name) DO UPDATE SET value = EXCLUDED.value`, name, value); err != nil {
		t.Fatalf("put setting %s: %v", name, err)
	}
}

// restoreSetting puts a shared settings row back the way the test found it.
func restoreSetting(t *testing.T, f *cacheFixture, name string) {
	t.Helper()
	var prev []byte
	err := f.direct.QueryRow(context.Background(), `SELECT value FROM settings WHERE name = $1`, name).Scan(&prev)
	t.Cleanup(func() {
		bg := context.Background()
		if err != nil {
			_, _ = f.direct.Exec(bg, `DELETE FROM settings WHERE name = $1`, name)
			return
		}
		_, _ = f.direct.Exec(bg, `UPDATE settings SET value = $2 WHERE name = $1`, name, prev)
	})
}
