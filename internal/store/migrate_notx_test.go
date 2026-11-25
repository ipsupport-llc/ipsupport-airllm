package store

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/testdb"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestSplitStatements(t *testing.T) {
	for _, tc := range []struct {
		name, in string
		want     []string
	}{
		{"plain", "SELECT 1; SELECT 2;", []string{"SELECT 1", "SELECT 2"}},
		{"no trailing semicolon", "SELECT 1;\nSELECT 2", []string{"SELECT 1", "SELECT 2"}},
		{"comment-only pieces dropped", "-- airllm:no-transaction\n-- a; b\n/* c; */\nSELECT 1;\n-- end;\n", []string{"-- airllm:no-transaction\n-- a; b\n/* c; */\nSELECT 1"}},
		{"nested block comment", "/* a /* b; */ c; */ SELECT 1; SELECT 2", []string{"/* a /* b; */ c; */ SELECT 1", "SELECT 2"}},
		{"string", "SELECT 'a;b', 'it''s; ok'; SELECT 2", []string{"SELECT 'a;b', 'it''s; ok'", "SELECT 2"}},
		{"escape string", `SELECT E'a\';b'; SELECT 2`, []string{`SELECT E'a\';b'`, "SELECT 2"}},
		{"quoted identifier", `SELECT 1 AS "x;y"; SELECT 2`, []string{`SELECT 1 AS "x;y"`, "SELECT 2"}},
		{"dollar quote", "DO $$ BEGIN PERFORM 1; END $$; SELECT 2", []string{"DO $$ BEGIN PERFORM 1; END $$", "SELECT 2"}},
		{"tagged dollar quote", "DO $fn$ BEGIN RAISE NOTICE '$$;'; END $fn$; SELECT 2", []string{"DO $fn$ BEGIN RAISE NOTICE '$$;'; END $fn$", "SELECT 2"}},
		{"positional param is not a dollar quote", "PREPARE p AS SELECT $1; SELECT 2", []string{"PREPARE p AS SELECT $1", "SELECT 2"}},
		{"empty", " \n-- nothing\n", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := splitStatements(tc.in); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("splitStatements(%q)\n got %q\nwant %q", tc.in, got, tc.want)
			}
		})
	}
}

// isolatedStore is a Store whose pool's search_path is a fresh schema of
// this package's test database, so a migrate run over test files keeps its
// own schema_migrations and tables away from the real schema.
func isolatedStore(t *testing.T, schema string) *Store {
	t.Helper()
	admin := testPool(t)
	ctx := context.Background()
	ident := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, `DROP SCHEMA IF EXISTS `+ident+` CASCADE; CREATE SCHEMA `+ident); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(context.Background(), `DROP SCHEMA IF EXISTS `+ident+` CASCADE`) })

	cfg, err := pgxpool.ParseConfig(testdb.DSN(t, func(ctx context.Context, p *pgxpool.Pool) error {
		return (&Store{PG: p}).Migrate(ctx)
	}))
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return &Store{PG: pool}
}

// indexValid reports whether index name exists in schema, and if so whether
// Postgres considers it valid.
func indexValid(t *testing.T, st *Store, schema, name string) (exists, valid bool) {
	t.Helper()
	err := st.PG.QueryRow(context.Background(), `
		SELECT i.indisvalid FROM pg_index i
		JOIN pg_class c ON c.oid = i.indexrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relname = $2`, schema, name).Scan(&valid)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false
	}
	if err != nil {
		t.Fatalf("look up index %s: %v", name, err)
	}
	return true, valid
}

func applied(t *testing.T, st *Store, version string) bool {
	t.Helper()
	var ok bool
	if err := st.PG.QueryRow(context.Background(),
		`SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version = $1)`, version).Scan(&ok); err != nil {
		t.Fatalf("check %s: %v", version, err)
	}
	return ok
}

func TestMigrateNoTransactionRunsCreateIndexConcurrently(t *testing.T) {
	const schema = "migrate_notx_ok"
	st := isolatedStore(t, schema)
	ctx := context.Background()

	files := fstest.MapFS{
		"0001_items.sql": {Data: []byte(`
			CREATE TABLE items (id int PRIMARY KEY, v int NOT NULL);
			INSERT INTO items SELECT g, g % 7 FROM generate_series(1, 1000) g;`)},
		"0002_items_v_idx.sql": {Data: []byte(noTxDirective + `
-- A comment; with a semicolon.
DROP INDEX CONCURRENTLY IF EXISTS items_v_idx;
CREATE INDEX CONCURRENTLY IF NOT EXISTS items_v_idx ON items (v);
`)},
	}
	if err := st.migrate(ctx, files); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if exists, valid := indexValid(t, st, schema, "items_v_idx"); !exists || !valid {
		t.Errorf("items_v_idx exists=%v valid=%v, want a valid index", exists, valid)
	}
	for _, v := range []string{"0001_items.sql", "0002_items_v_idx.sql"} {
		if !applied(t, st, v) {
			t.Errorf("%s not recorded in schema_migrations", v)
		}
	}
	if err := st.migrate(ctx, files); err != nil {
		t.Errorf("second migrate (nothing pending) = %v", err)
	}

	// Control: without the directive the same statement runs in a
	// transaction, which Postgres refuses, and nothing is recorded.
	files["0003_in_tx.sql"] = &fstest.MapFile{Data: []byte(`CREATE INDEX CONCURRENTLY items_id_v_idx ON items (id, v);`)}
	if err := st.migrate(ctx, files); err == nil || !strings.Contains(err.Error(), "transaction block") {
		t.Errorf("CONCURRENTLY inside a transaction = %v, want a transaction-block error", err)
	}
	if applied(t, st, "0003_in_tx.sql") {
		t.Error("failed 0003_in_tx.sql was recorded")
	}
}

// TestMigrateNoTransactionRecoversFromInvalidIndex follows the documented
// pattern: a failed CREATE INDEX CONCURRENTLY leaves an INVALID index, the
// file is not recorded, and the next run drops and rebuilds it.
func TestMigrateNoTransactionRecoversFromInvalidIndex(t *testing.T) {
	const schema = "migrate_notx_retry"
	st := isolatedStore(t, schema)
	ctx := context.Background()

	files := fstest.MapFS{
		"0001_items.sql": {Data: []byte(`
			CREATE TABLE items (id int PRIMARY KEY, v int NOT NULL);
			INSERT INTO items VALUES (1, 1), (2, 1), (3, 2);`)},
		"0002_items_v_key.sql": {Data: []byte(noTxDirective + `
DROP INDEX CONCURRENTLY IF EXISTS items_v_key;
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS items_v_key ON items (v);
`)},
	}
	if err := st.migrate(ctx, files); err == nil {
		t.Fatal("migrate succeeded over duplicate values, want a unique violation")
	}
	if exists, valid := indexValid(t, st, schema, "items_v_key"); !exists || valid {
		t.Fatalf("after the failed build: exists=%v valid=%v, want an INVALID index left behind", exists, valid)
	}
	if applied(t, st, "0002_items_v_key.sql") {
		t.Fatal("failed 0002_items_v_key.sql was recorded")
	}

	if _, err := st.PG.Exec(ctx, `DELETE FROM items WHERE id = 2`); err != nil {
		t.Fatalf("remove duplicate: %v", err)
	}
	if err := st.migrate(ctx, files); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if exists, valid := indexValid(t, st, schema, "items_v_key"); !exists || !valid {
		t.Errorf("after the retry: exists=%v valid=%v, want a valid index", exists, valid)
	}
	if !applied(t, st, "0002_items_v_key.sql") {
		t.Error("0002_items_v_key.sql not recorded after the retry")
	}
}
