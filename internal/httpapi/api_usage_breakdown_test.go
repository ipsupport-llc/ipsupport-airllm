package httpapi

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// testPool connects to TEST_DATABASE_URL or skips. The DB must have the
// migrations applied (run the dev compose stack: make compose-up).
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping breakdown integration test")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// TestUsageBreakdownQueries exercises breakdownProviderQuery and
// breakdownModelQuery directly (not through usageBreakdown, which queries
// the pool rather than a tx) so the whole test runs inside one rolled-back
// transaction and leaves no rows behind.
func TestUsageBreakdownQueries(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)

	mustExec := func(sql string, args ...any) {
		t.Helper()
		if _, err := tx.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %s: %v", sql, err)
		}
	}

	insert := `INSERT INTO usage_ledger
		(alias, provider_name, upstream_model, prompt_tokens, completion_tokens, reasoning_tokens, cost_usd, status, latency_ms)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`

	// bp-openai / alias-a: 3 rows, one erroring (status 500), total cost 3.50.
	// The middle row spent 40 of its 100 output tokens thinking; the other two
	// did none, which is how a mixed model's traffic actually looks.
	mustExec(insert, "alias-a", "bp-openai", "gpt-4", 100, 50, 0, 1.00, 200, 100)
	mustExec(insert, "alias-a", "bp-openai", "gpt-4", 200, 100, 40, 2.00, 200, 200)
	mustExec(insert, "alias-a", "bp-openai", "gpt-4", 50, 25, 0, 0.50, 500, 300)

	// bp-mock / alias-b: 1 row, higher cost than bp-openai's total (5.00 > 3.50)
	// so it must sort first under ORDER BY cost DESC.
	mustExec(insert, "alias-b", "bp-mock", "mock-1", 10, 10, 0, 5.00, 200, 50)

	// Row with an empty provider_name: a request that exhausted every target
	// (ledgered as a failure with no provider). Must be excluded from the
	// provider breakdown but VISIBLE in the model breakdown — otherwise
	// failures disappear from the report.
	mustExec(insert, "alias-c", "", "untracked", 999, 999, 0, 100.00, 502, 10)

	// Scope to this test's fixture providers via the where-clause injection
	// point (mirroring how handleUsageBreakdown adds "AND user_id = $2") so
	// pre-existing rows in the shared dev database (e.g. provider "mock" from
	// real usage) don't leak into the assertions.
	const scopeToFixtures = `AND provider_name IN ('bp-openai', 'bp-mock')`

	// --- provider breakdown ---
	provRows, err := tx.Query(ctx, fmt.Sprintf(breakdownProviderQuery, scopeToFixtures), 24)
	if err != nil {
		t.Fatalf("provider query: %v", err)
	}
	var provs []providerUsage
	for provRows.Next() {
		var p providerUsage
		if err := provRows.Scan(&p.Provider, &p.Requests, &p.TokensIn, &p.TokensOut, &p.TokensReasoning, &p.CostUSD, &p.P95ms, &p.Errors); err != nil {
			provRows.Close()
			t.Fatalf("scan provider row: %v", err)
		}
		provs = append(provs, p)
	}
	provRows.Close()
	if err := provRows.Err(); err != nil {
		t.Fatalf("provider rows err: %v", err)
	}

	if len(provs) != 2 {
		t.Fatalf("providers = %d groups, want 2 (got %+v)", len(provs), provs)
	}
	// cost DESC: bp-mock (5.00) before bp-openai (3.50).
	if provs[0].Provider != "bp-mock" || provs[1].Provider != "bp-openai" {
		t.Errorf("provider order = [%s, %s], want [bp-mock, bp-openai]", provs[0].Provider, provs[1].Provider)
	}
	for _, p := range provs {
		if p.Provider == "" {
			t.Errorf("empty-provider row leaked into results: %+v", p)
		}
	}
	var openai providerUsage
	for _, p := range provs {
		if p.Provider == "bp-openai" {
			openai = p
		}
	}
	if openai.Requests != 3 {
		t.Errorf("bp-openai requests = %d, want 3", openai.Requests)
	}
	// Prompt and completion tokens are priced differently - the breakdown
	// must report them separately (350 in / 175 out for the fixtures).
	if openai.TokensIn != 350 || openai.TokensOut != 175 {
		t.Errorf("bp-openai tokens = %d in / %d out, want 350/175", openai.TokensIn, openai.TokensOut)
	}
	// The thinking share aggregates as its own sum and stays INSIDE the 175
	// output tokens above: an operator reading the two columns is reading one
	// quantity and a part of it, not two quantities to add.
	if openai.TokensReasoning != 40 {
		t.Errorf("bp-openai thinking tokens = %d, want 40", openai.TokensReasoning)
	}
	if openai.TokensReasoning > openai.TokensOut {
		t.Errorf("thinking tokens (%d) exceed output tokens (%d); the breakdown is no longer a share of the output",
			openai.TokensReasoning, openai.TokensOut)
	}
	if openai.Errors != 1 {
		t.Errorf("bp-openai errors = %d, want 1", openai.Errors)
	}

	// --- model breakdown ---
	// The model query has no provider filter, so scope by the fixture aliases
	// to keep the failed (empty-provider) row in view.
	const scopeToFixtureAliases = `AND alias IN ('alias-a', 'alias-b', 'alias-c')`
	modelRows, err := tx.Query(ctx, fmt.Sprintf(breakdownModelQuery, scopeToFixtureAliases), 24)
	if err != nil {
		t.Fatalf("model query: %v", err)
	}
	var models []modelUsage
	for modelRows.Next() {
		var m modelUsage
		if err := modelRows.Scan(&m.Alias, &m.Provider, &m.UpstreamModel, &m.Requests, &m.TokensIn, &m.TokensOut, &m.TokensReasoning, &m.CostUSD, &m.P95ms, &m.Errors); err != nil {
			modelRows.Close()
			t.Fatalf("scan model row: %v", err)
		}
		models = append(models, m)
	}
	modelRows.Close()
	if err := modelRows.Err(); err != nil {
		t.Fatalf("model rows err: %v", err)
	}

	if len(models) != 3 {
		t.Fatalf("models = %d groups, want 3 (got %+v)", len(models), models)
	}
	// cost DESC: the failed no-provider row (100.00) first, then alias-b (5.00).
	if models[0].Alias != "alias-c" || models[0].Provider != "" || models[0].Errors != 1 {
		t.Errorf("top model = %+v, want alias-c with empty provider and errors=1 (failures must stay visible)", models[0])
	}
	if models[1].Alias != "alias-b" || models[1].Provider != "bp-mock" || models[1].UpstreamModel != "mock-1" {
		t.Errorf("second model = %+v, want alias-b/bp-mock/mock-1 (cost DESC)", models[1])
	}
	var gotOpenaiModel bool
	for _, m := range models {
		if m.Alias == "alias-a" && m.Provider == "bp-openai" && m.UpstreamModel == "gpt-4" {
			gotOpenaiModel = true
			if m.Requests != 3 {
				t.Errorf("alias-a model requests = %d, want 3", m.Requests)
			}
			if m.TokensReasoning != 40 {
				t.Errorf("alias-a model thinking tokens = %d, want 40", m.TokensReasoning)
			}
		}
	}
	if !gotOpenaiModel {
		t.Errorf("alias-a/bp-openai/gpt-4 group missing from %+v", models)
	}
}

// TestRecentRequestsQuery exercises recentRequestsQuery directly (mirroring
// TestUsageBreakdownQueries) inside one rolled-back transaction.
func TestRecentRequestsQuery(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)

	// Explicit, distinct ts values: both rows would otherwise default to the
	// same now() inside one transaction, making ORDER BY ts DESC ambiguous.
	insert := `INSERT INTO usage_ledger
		(ts, alias, provider_name, upstream_model, prompt_tokens, completion_tokens, reasoning_tokens, cost_usd, status, latency_ms, error)
		VALUES (now() - interval '1 second', $1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`
	if _, err := tx.Exec(ctx, insert, "rr-alias-a", "rr-openai", "gpt-4", 100, 50, 20, 1.00, 200, 120, ""); err != nil {
		t.Fatalf("insert older row: %v", err)
	}
	insertNewer := `INSERT INTO usage_ledger
		(ts, alias, provider_name, upstream_model, prompt_tokens, completion_tokens, reasoning_tokens, cost_usd, status, latency_ms, error)
		VALUES (now(), $1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`
	if _, err := tx.Exec(ctx, insertNewer, "rr-alias-b", "", "", 0, 0, 0, 0, 502, 30, "provider busy"); err != nil {
		t.Fatalf("insert failed row: %v", err)
	}

	rows, err := tx.Query(ctx, recentRequestsQuery, 2)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	var got []recentRequest
	for rows.Next() {
		var req recentRequest
		if err := rows.Scan(&req.Ts, &req.Alias, &req.Provider, &req.UpstreamModel, &req.Status,
			&req.LatencyMS, &req.TokensIn, &req.TokensOut, &req.TokensReasoning, &req.CostUSD,
			&req.ErrorMsg); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, req)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows err: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2 (LIMIT $1 with $1=2): %+v", len(got), got)
	}
	// ORDER BY ts DESC: the row inserted second (rr-alias-b) comes first.
	if got[0].Alias != "rr-alias-b" || got[0].Status != 502 || got[0].ErrorMsg != "provider busy" {
		t.Errorf("newest row = %+v, want rr-alias-b/502/provider busy", got[0])
	}
	if got[0].Provider != "" || got[0].UpstreamModel != "" {
		t.Errorf("failed row must keep its empty provider/model visible, got %+v", got[0])
	}
	if got[1].Alias != "rr-alias-a" || got[1].Provider != "rr-openai" || got[1].Status != 200 {
		t.Errorf("older row = %+v, want rr-alias-a/rr-openai/200", got[1])
	}
	if got[1].TokensReasoning != 20 {
		t.Errorf("older row thinking tokens = %d, want 20", got[1].TokensReasoning)
	}
}
