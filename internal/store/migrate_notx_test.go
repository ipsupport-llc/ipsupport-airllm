package store

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/testdb"
	"github.com/ipsupport-llc/ipsupport-airllm/migrations"
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
		{"escape string with doubled quote", `SELECT E'a''b\';c'; SELECT 2;`, []string{`SELECT E'a''b\';c'`, "SELECT 2"}},
		{"escape string ending in a backslash", `SELECT E'a\\'; SELECT 2`, []string{`SELECT E'a\\'`, "SELECT 2"}},
		{"escape string continued on the next line", "SELECT E'a'\n'b\\';c';\nSELECT 2;", []string{"SELECT E'a'\n'b\\';c'", "SELECT 2"}},
		{"escape string continued past a comment line", "SELECT E'a'  \n-- note\n\t'b\\';c'; SELECT 2", []string{"SELECT E'a'  \n-- note\n\t'b\\';c'", "SELECT 2"}},
		// Checked against PostgreSQL 18: -- comments may sit anywhere in the
		// separator and a CR alone is a newline; a /* */ comment ends the
		// string instead, so 'b\' is a new standard string and ; splits.
		{"continued after a comment on the same line", "SELECT E'a' -- n\n'b\\';c'; SELECT 2", []string{"SELECT E'a' -- n\n'b\\';c'", "SELECT 2"}},
		{"continued after a CR-terminated comment", "SELECT E'a' -- n\r'b\\';c'; SELECT 2", []string{"SELECT E'a' -- n\r'b\\';c'", "SELECT 2"}},
		{"continued on a CR-only line", "SELECT E'a'\r'b\\';c';\rSELECT 2", []string{"SELECT E'a'\r'b\\';c'", "SELECT 2"}},
		{"continued with vertical tab and form feed", "SELECT E'a'\v\f\n\v'b\\';c'; SELECT 2", []string{"SELECT E'a'\v\f\n\v'b\\';c'", "SELECT 2"}},
		{"block comment after the newline ends the string", "SELECT E'a'\n/* x */'b\\';c'", []string{"SELECT E'a'\n/* x */'b\\'", "c'"}},
		{"block comment holding the newline ends the string", "SELECT E'a' /* x\n */'b\\';c'", []string{"SELECT E'a' /* x\n */'b\\'", "c'"}},
		{"no newline, no continuation", "SELECT E'a' 'b\\';c'", []string{"SELECT E'a' 'b\\'", "c'"}},
		{"line comment ends at CR", "SELECT 1 -- x\r; SELECT 2", []string{"SELECT 1 -- x", "SELECT 2"}},
		{"standard string continued on the next line", "SELECT 'a'\n'b;c'; SELECT 2", []string{"SELECT 'a'\n'b;c'", "SELECT 2"}},
		{"standard string with doubled quotes", "SELECT 'x'';y''', ''';'; SELECT 2", []string{"SELECT 'x'';y''', ''';'", "SELECT 2"}},
		{"standard string keeps backslash literal", `SELECT 'a\'; SELECT 2`, []string{`SELECT 'a\'`, "SELECT 2"}},
		{"quoted identifier with doubled quote", `SELECT 1 AS "a"";b"; SELECT 2`, []string{`SELECT 1 AS "a"";b"`, "SELECT 2"}},
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
	if err := st.migrate(ctx, files, nil); err != nil {
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
	if err := st.migrate(ctx, files, nil); err != nil {
		t.Errorf("second migrate (nothing pending) = %v", err)
	}

	// Control: without the directive the same statement runs in a
	// transaction, which Postgres refuses, and nothing is recorded.
	files["0003_in_tx.sql"] = &fstest.MapFile{Data: []byte(`CREATE INDEX CONCURRENTLY items_id_v_idx ON items (id, v);`)}
	if err := st.migrate(ctx, files, nil); err == nil || !strings.Contains(err.Error(), "transaction block") {
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
	if err := st.migrate(ctx, files, nil); err == nil {
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
	if err := st.migrate(ctx, files, nil); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if exists, valid := indexValid(t, st, schema, "items_v_key"); !exists || !valid {
		t.Errorf("after the retry: exists=%v valid=%v, want a valid index", exists, valid)
	}
	if !applied(t, st, "0002_items_v_key.sql") {
		t.Error("0002_items_v_key.sql not recorded after the retry")
	}
}

// TestMigrateNotifyCallsOnLockedOnlyOnceTheLockIsHeld: the boot phase that
// keeps the pod live is entered through onLocked, so it must not fire while
// Migrate still waits for another process's lock, and must fire before any
// file is applied.
func TestMigrateNotifyCallsOnLockedOnlyOnceTheLockIsHeld(t *testing.T) {
	st := isolatedStore(t, "migrate_onlocked")
	ctx := context.Background()

	held, err := st.PG.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire held conn: %v", err)
	}
	defer held.Release()
	if _, err := held.Exec(ctx, `SELECT pg_advisory_lock($1)`, int64(migrationLockKey)); err != nil {
		t.Fatalf("take the lock: %v", err)
	}

	locked := make(chan bool, 1) // whether the items table was still missing
	files := fstest.MapFS{"0001_items.sql": {Data: []byte(`CREATE TABLE items (id int PRIMARY KEY);`)}}
	done := make(chan error, 1)
	go func() {
		done <- st.migrate(ctx, files, func() {
			var missing bool
			_ = st.PG.QueryRow(ctx, `SELECT to_regclass('items') IS NULL`).Scan(&missing)
			locked <- missing
		})
	}()

	select {
	case <-locked:
		t.Fatal("onLocked ran while another connection held the migration lock")
	case err := <-done:
		t.Fatalf("migrate returned (%v) while the lock was held", err)
	case <-time.After(200 * time.Millisecond):
	}

	if _, err := held.Exec(ctx, `SELECT pg_advisory_unlock($1)`, int64(migrationLockKey)); err != nil {
		t.Fatalf("release the lock: %v", err)
	}
	select {
	case missing := <-locked:
		if !missing {
			t.Error("onLocked ran after a migration was applied, want before")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("onLocked never ran after the lock was released")
	}
	if err := <-done; err != nil {
		t.Fatalf("migrate: %v", err)
	}
}

// sqlTokens is stmt's tokens with comments and whitespace dropped, using the
// migrator's own lexer: a string or quoted name is one token, and so is each
// of ( ) , — everything else runs together between those.
func sqlTokens(stmt string) []string {
	var toks []string
	for _, seg := range lexSQL(stmt) {
		switch seg.kind {
		case segQuoted:
			toks = append(toks, seg.text)
		case segCode:
			word := ""
			for _, r := range seg.text {
				if strings.ContainsRune("(),", r) {
					if word != "" {
						toks = append(toks, word)
					}
					toks, word = append(toks, string(r)), ""
					continue
				}
				word += string(r)
			}
			if word != "" {
				toks = append(toks, word)
			}
		}
	}
	return toks
}

// indexName folds an unqualified index name the way Postgres does: a
// quoted name keeps its case, an unquoted one is lowercased.
func indexName(raw string) string {
	if len(raw) > 1 && strings.HasPrefix(raw, `"`) && strings.HasSuffix(raw, `"`) {
		return strings.ReplaceAll(raw[1:len(raw)-1], `""`, `"`)
	}
	return strings.ToLower(raw)
}

// qualified reports whether the name at toks[i] carries a schema: a dot in
// an unquoted token (public.x, public."X"), or a following token that
// starts with one ("s".x, "s"."x").
func qualified(toks []string, i int) bool {
	if i >= len(toks) {
		return false
	}
	if !strings.HasPrefix(toks[i], `"`) && strings.Contains(toks[i], ".") {
		return true
	}
	return i+1 < len(toks) && strings.HasPrefix(toks[i+1], ".")
}

func hasKeywords(toks []string, words ...string) bool {
	if len(toks) < len(words) {
		return false
	}
	for i, w := range words {
		if !strings.EqualFold(toks[i], w) {
			return false
		}
	}
	return true
}

const noTxShape = "a no-transaction migration must be exactly `DROP INDEX CONCURRENTLY IF EXISTS <name>; CREATE [UNIQUE] INDEX CONCURRENTLY IF NOT EXISTS <name> ON ...`"

// checkNoTxFile enforces the only shape a no-transaction migration may
// have (see noTxDirective): DROP INDEX CONCURRENTLY IF EXISTS <name>, then
// one CREATE [UNIQUE] INDEX CONCURRENTLY IF NOT EXISTS <name>, nothing else.
func checkNoTxFile(sql string) error {
	stmts := splitStatements(sql)
	if len(stmts) != 2 {
		return fmt.Errorf("%d statements; %s", len(stmts), noTxShape)
	}
	drop := sqlTokens(stmts[0])
	if hasKeywords(drop, "DROP", "INDEX", "CONCURRENTLY", "IF", "EXISTS") && qualified(drop, 5) {
		return fmt.Errorf("first statement %q: the index name must be unqualified (no schema)", stmts[0])
	}
	if len(drop) != 6 || !hasKeywords(drop, "DROP", "INDEX", "CONCURRENTLY", "IF", "EXISTS") {
		return fmt.Errorf("first statement %q; %s", stmts[0], noTxShape)
	}
	create := sqlTokens(stmts[1])
	if len(create) > 1 && strings.EqualFold(create[1], "UNIQUE") {
		create = append([]string{create[0]}, create[2:]...)
	}
	if hasKeywords(create, "CREATE", "INDEX", "CONCURRENTLY", "IF", "NOT", "EXISTS") && qualified(create, 6) {
		return fmt.Errorf("second statement %q: the index name must be unqualified (no schema)", stmts[1])
	}
	if len(create) < 8 || !hasKeywords(create, "CREATE", "INDEX", "CONCURRENTLY", "IF", "NOT", "EXISTS") || !strings.EqualFold(create[7], "ON") {
		return fmt.Errorf("second statement %q; %s", stmts[1], noTxShape)
	}
	if d, c := indexName(drop[5]), indexName(create[6]); d != c {
		return fmt.Errorf("drops index %s but creates %s; %s", d, c, noTxShape)
	}
	return nil
}

func TestCheckNoTxFile(t *testing.T) {
	const drop = "DROP INDEX CONCURRENTLY IF EXISTS a_idx;\n"
	for _, tc := range []struct {
		name, sql string
		ok        bool
	}{
		{"canonical", noTxDirective + "\n" + drop + "CREATE INDEX CONCURRENTLY IF NOT EXISTS a_idx ON t (a);\n", true},
		{"unique, lower case, qualified table", "drop index concurrently if exists A_IDX;\ncreate unique index concurrently if not exists a_idx on public.t(a) where a > 0;", true},
		{"CR-only line endings", "-- airllm:no-transaction\r" + "DROP INDEX CONCURRENTLY IF EXISTS a_idx;\r-- note\rCREATE INDEX CONCURRENTLY IF NOT EXISTS a_idx ON t (a);\r", true},
		{"CR-terminated comment can't hide a statement", "DROP INDEX CONCURRENTLY IF EXISTS a_idx; -- x\rALTER TABLE t ADD COLUMN b int;\nCREATE INDEX CONCURRENTLY IF NOT EXISTS a_idx ON t (a);", false},
		{"quoted name", `DROP INDEX CONCURRENTLY IF EXISTS "A idx"; CREATE INDEX CONCURRENTLY IF NOT EXISTS "A idx" ON t (a);`, true},
		{"comments anywhere", "-- why\n" + drop + "CREATE /* x; */ INDEX -- y\n CONCURRENTLY IF NOT EXISTS a_idx ON t (a); -- done\n", true},

		{"no drop", "CREATE INDEX CONCURRENTLY IF NOT EXISTS a_idx ON t (a);", false},
		{"create without IF NOT EXISTS", drop + "CREATE INDEX CONCURRENTLY a_idx ON t (a);", false},
		{"drop after create", "CREATE INDEX CONCURRENTLY IF NOT EXISTS a_idx ON t (a);\n" + drop, false},
		{"drop of another index", "DROP INDEX CONCURRENTLY IF EXISTS b_idx;\nCREATE INDEX CONCURRENTLY IF NOT EXISTS a_idx ON t (a);", false},
		{"drop only in a line comment", "-- DROP INDEX CONCURRENTLY IF EXISTS a_idx;\nCREATE INDEX CONCURRENTLY IF NOT EXISTS a_idx ON t (a);", false},
		{"drop only in a block comment", "/* DROP INDEX CONCURRENTLY IF EXISTS a_idx; */ CREATE INDEX CONCURRENTLY IF NOT EXISTS a_idx ON t (a);", false},
		{"nested block comment hides the create", drop + "/* /* */ CREATE INDEX CONCURRENTLY IF NOT EXISTS a_idx ON t (a); */ ALTER TABLE t ADD COLUMN b int;", false},
		{"comment between keywords hides a missing drop", "CREATE /**/ INDEX CONCURRENTLY IF NOT EXISTS a_idx ON t (a);", false},
		{"drop without IF EXISTS", "DROP INDEX CONCURRENTLY a_idx;\nCREATE INDEX CONCURRENTLY IF NOT EXISTS a_idx ON t (a);", false},
		{"quoted name differs in case", `DROP INDEX CONCURRENTLY IF EXISTS a_idx; CREATE INDEX CONCURRENTLY IF NOT EXISTS "A_idx" ON t (a);`, false},
		{"unnamed", drop + "CREATE INDEX CONCURRENTLY ON t (a);", false},
		{"extra ALTER", drop + "CREATE INDEX CONCURRENTLY IF NOT EXISTS a_idx ON t (a);\nALTER TABLE t ADD COLUMN b int;", false},
		{"only an ALTER", "ALTER TABLE t ADD COLUMN IF NOT EXISTS b int;", false},
		{"two indexes", drop + "CREATE INDEX CONCURRENTLY IF NOT EXISTS a_idx ON t (a);\nDROP INDEX CONCURRENTLY IF EXISTS b_idx;\nCREATE INDEX CONCURRENTLY IF NOT EXISTS b_idx ON t (b);", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := checkNoTxFile(tc.sql); (err == nil) != tc.ok {
				t.Errorf("checkNoTxFile = %v, want ok=%v", err, tc.ok)
			}
		})
	}
}

func TestCheckNoTxFileRejectsQualifiedIndexNames(t *testing.T) {
	const create = "CREATE INDEX CONCURRENTLY IF NOT EXISTS a_idx ON t (a);"
	for _, sql := range []string{
		"DROP INDEX CONCURRENTLY IF EXISTS public.a_idx;\n" + create,
		// Would drop another schema's index and leave an INVALID a_idx.
		"DROP INDEX CONCURRENTLY IF EXISTS other.a_idx;\n" + create,
		`DROP INDEX CONCURRENTLY IF EXISTS "S"."A";` + "\n" + `CREATE INDEX CONCURRENTLY IF NOT EXISTS "A" ON t (a);`,
		`DROP INDEX CONCURRENTLY IF EXISTS public."A";` + "\n" + `CREATE INDEX CONCURRENTLY IF NOT EXISTS "A" ON t (a);`,
		`DROP INDEX CONCURRENTLY IF EXISTS "s".a_idx;` + "\n" + create,
		"DROP INDEX CONCURRENTLY IF EXISTS a_idx;\nCREATE INDEX CONCURRENTLY IF NOT EXISTS public.a_idx ON t (a);",
	} {
		if err := checkNoTxFile(sql); err == nil || !strings.Contains(err.Error(), "unqualified") {
			t.Errorf("checkNoTxFile(%q) = %v, want an unqualified-name error", sql, err)
		}
	}
}

// TestNoTxMigrationsDropIndexFirst gates every real no-transaction migration
// on checkNoTxFile (see noTxDirective).
func TestNoTxMigrationsDropIndexFirst(t *testing.T) {
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		b, err := fs.ReadFile(migrations.FS, e.Name())
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		if first, _, _ := strings.Cut(string(b), "\n"); strings.TrimSuffix(first, "\r") != noTxDirective {
			continue
		}
		if err := checkNoTxFile(string(b)); err != nil {
			t.Errorf("%s: %v", e.Name(), err)
		}
	}
}
