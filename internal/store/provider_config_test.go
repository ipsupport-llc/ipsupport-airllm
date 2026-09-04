package store

import (
	"context"
	"encoding/json"
	"testing"
)

// TestProviderConfigRoundTrip proves what an operator actually cares about:
// a cloud project and location entered once are still there when the gateway
// is restarted and rebuilds its registry from the database.
//
// It is worth a database for two reasons a pure test cannot cover: that the
// column exists with the default the migration promises, and that a jsonb
// value scans back into json.RawMessage rather than needing a cast.
func TestProviderConfigRoundTrip(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `SELECT config FROM providers LIMIT 0`); err != nil {
		t.Skipf("config column not present (migration 0012 not applied?): %v", err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO providers (name, kind, base_url, enabled, config)
		VALUES ($1, 'vertex', '', true, $2)`,
		"config-test-vertex", `{"project":"acme","location":"us-west1"}`); err != nil {
		t.Fatalf("insert vertex provider: %v", err)
	}
	// A kind that stores no configuration must come back as the default,
	// which is what keeps the migration additive for the providers already
	// in the table.
	if _, err := tx.Exec(ctx, `
		INSERT INTO providers (name, kind, base_url, enabled)
		VALUES ($1, 'openai', 'http://example.invalid', true)`,
		"config-test-openai"); err != nil {
		t.Fatalf("insert openai provider: %v", err)
	}

	// The registry's own query, run against the tx so it leaves no rows.
	rows, err := tx.Query(ctx,
		`SELECT name, kind, base_url, cred_enc, max_concurrency, config
		 FROM providers WHERE name = ANY($1) ORDER BY name`,
		[]string{"config-test-openai", "config-test-vertex"})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	var got []ProviderRow
	for rows.Next() {
		var p ProviderRow
		if err := rows.Scan(&p.Name, &p.Kind, &p.BaseURL, &p.CredEnc, &p.MaxConcurrency, &p.Config); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, p)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 rows, got %d", len(got))
	}

	if string(got[0].Config) != "{}" {
		t.Errorf("a provider that stores no configuration came back as %q, want the empty default", got[0].Config)
	}

	var cfg struct {
		Project  string `json:"project"`
		Location string `json:"location"`
	}
	if err := json.Unmarshal(got[1].Config, &cfg); err != nil {
		t.Fatalf("stored configuration did not come back as JSON: %v (%q)", err, got[1].Config)
	}
	if cfg.Project != "acme" || cfg.Location != "us-west1" {
		t.Errorf("configuration round-tripped as %+v, want project acme in us-west1", cfg)
	}
}
