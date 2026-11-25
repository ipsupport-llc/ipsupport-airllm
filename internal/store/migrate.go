package store

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strings"

	"github.com/ipsupport-llc/ipsupport-airllm/migrations"
)

// migrationLockKey is a fixed, arbitrary key for the session-level advisory
// lock Migrate holds for its duration. Any constant works as long as every
// process uses the same one.
const migrationLockKey = 72_70_01

// noTxDirective, as the exact first line of a migration file, makes Migrate
// run that file outside a transaction (still under the advisory lock), one
// statement at a time, and record it in schema_migrations afterwards. It
// exists for statements Postgres refuses inside a transaction block, chiefly
// CREATE INDEX CONCURRENTLY, which builds an index on a big table without
// blocking writes for the whole build.
//
// A failure part-way leaves whatever already ran in place, and the file is
// rerun from the top on the next boot, so every statement in such a file
// must be idempotent (IF NOT EXISTS / IF EXISTS). A failed or interrupted
// CREATE INDEX CONCURRENTLY also leaves an INVALID index behind, which
// IF NOT EXISTS would then skip — drop it first:
//
//	-- airllm:no-transaction
//	DROP INDEX CONCURRENTLY IF EXISTS idx_usage_ledger_alias_ts;
//	CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_usage_ledger_alias_ts
//	    ON usage_ledger (alias, ts);
//
// The DROP also rebuilds an index that a previous attempt finished without
// getting to record the file, which is only time lost. Keep to one index per
// file. See docs/operations.md#migrations.
const noTxDirective = "-- airllm:no-transaction"

// Migrate applies any embedded *.sql migrations not yet recorded, in
// lexicographic order, each inside its own transaction unless it starts
// with noTxDirective.
//
// The whole run holds a Postgres advisory lock on one dedicated connection,
// serializing it across every process that might call Migrate at the same
// time — a multi-replica rolling deploy starts several pods on the new
// image concurrently (this chart's HPA already scales the app beyond one
// replica). Without the lock, two processes can both see a migration as
// not-yet-applied (the schema_migrations check) and run its SQL
// concurrently: at best a harmless duplicate-application error, at worst a
// deadlock or a double-applied non-idempotent statement. A session-level
// lock (not pg_advisory_xact_lock) is used deliberately, pinned to one
// Acquire'd connection for the function's whole lifetime — advisory locks
// are owned by the connection that took them, and a pool connection
// bounces between callers after every individual Exec/QueryRow, so taking
// the lock through the pool directly would risk releasing (or never
// releasing) it on a different physical connection than the one that holds
// it.
func (s *Store) Migrate(ctx context.Context) error {
	return s.migrate(ctx, migrations.FS)
}

// migrate is Migrate over an arbitrary set of migration files, so tests can
// supply their own without touching the real migrations directory.
func (s *Store) migrate(ctx context.Context, fsys fs.FS) error {
	conn, err := s.PG.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration connection: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, int64(migrationLockKey)); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	defer func() {
		// A fresh context: the caller's ctx may already be done by the time
		// we get here, but the unlock must still run to release the lock.
		if _, err := conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, int64(migrationLockKey)); err != nil {
			slog.Error("migration: failed to release advisory lock", "err", err)
		}
	}()

	if _, err := conn.Exec(ctx,
		`CREATE TABLE IF NOT EXISTS schema_migrations (
			version    text PRIMARY KEY,
			applied_at timestamptz NOT NULL DEFAULT now()
		)`); err != nil {
		return fmt.Errorf("ensure schema_migrations: %w", err)
	}

	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return fmt.Errorf("read migrations dir: %w", err)
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)

	for _, f := range files {
		var applied bool
		if err := conn.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version = $1)`, f,
		).Scan(&applied); err != nil {
			return fmt.Errorf("check migration %s: %w", f, err)
		}
		if applied {
			continue
		}

		sqlBytes, err := fs.ReadFile(fsys, f)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", f, err)
		}

		if firstLine, _, _ := strings.Cut(string(sqlBytes), "\n"); strings.TrimSuffix(firstLine, "\r") == noTxDirective {
			// Sent one by one: a multi-statement query string is an implicit
			// transaction block, which CONCURRENTLY refuses just the same.
			for _, stmt := range splitStatements(string(sqlBytes)) {
				if _, err := conn.Exec(ctx, stmt); err != nil {
					return fmt.Errorf("apply %s (no transaction): %w", f, err)
				}
			}
			if _, err := conn.Exec(ctx,
				`INSERT INTO schema_migrations (version) VALUES ($1)`, f); err != nil {
				return fmt.Errorf("record %s: %w", f, err)
			}
			slog.Info("migration applied", "version", f, "transaction", false)
			continue
		}

		tx, err := conn.Begin(ctx)
		if err != nil {
			return fmt.Errorf("begin %s: %w", f, err)
		}
		if _, err := tx.Exec(ctx, string(sqlBytes)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("apply %s: %w", f, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO schema_migrations (version) VALUES ($1)`, f); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("record %s: %w", f, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit %s: %w", f, err)
		}
		slog.Info("migration applied", "version", f)
	}
	return nil
}

// splitStatements splits a SQL script into its statements on top-level
// semicolons, skipping those inside quotes, dollar quotes and comments.
// Statements that hold nothing but comments and whitespace are dropped.
func splitStatements(script string) []string {
	var out []string
	start, hasCode := 0, false
	flush := func(end int) {
		if hasCode {
			out = append(out, strings.TrimSpace(script[start:end]))
		}
		start, hasCode = end+1, false
	}
	for i := 0; i < len(script); i++ {
		c := script[i]
		switch {
		case c == ';':
			flush(i)
		case c == '-' && strings.HasPrefix(script[i:], "--"):
			if n := strings.IndexByte(script[i:], '\n'); n >= 0 {
				i += n
			} else {
				i = len(script)
			}
		case c == '/' && strings.HasPrefix(script[i:], "/*"):
			// Block comments nest in Postgres.
			depth := 0
			for ; i < len(script); i++ {
				if strings.HasPrefix(script[i:], "/*") {
					depth++
					i++
				} else if strings.HasPrefix(script[i:], "*/") {
					depth--
					i++
					if depth == 0 {
						break
					}
				}
			}
		case c == '\'' || c == '"':
			// A doubled quote closes and reopens, which comes out the same.
			// E'...' strings also escape with a backslash.
			escapes := c == '\'' && i > 0 && (script[i-1] == 'E' || script[i-1] == 'e') && (i < 2 || !isIdentByte(script[i-2]))
			hasCode = true
			for i++; i < len(script) && script[i] != c; i++ {
				if escapes && script[i] == '\\' {
					i++
				}
			}
		case c == '$' && (i == 0 || !isIdentByte(script[i-1])):
			hasCode = true
			if tag, ok := dollarTag(script[i:]); ok {
				if n := strings.Index(script[i+len(tag):], tag); n >= 0 {
					i += len(tag) + n + len(tag) - 1
				} else {
					i = len(script)
				}
			}
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
		default:
			hasCode = true
		}
	}
	flush(len(script))
	return out
}

// dollarTag returns the opening $tag$ (or $$) at the start of s, if any.
func dollarTag(s string) (string, bool) {
	for j := 1; j < len(s); j++ {
		switch c := s[j]; {
		case c == '$':
			return s[:j+1], true
		case c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80:
		case c >= '0' && c <= '9' && j > 1:
		default:
			return "", false
		}
	}
	return "", false
}

func isIdentByte(c byte) bool {
	return c == '_' || c == '$' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c >= 0x80
}
