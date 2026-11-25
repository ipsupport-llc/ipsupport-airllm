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
// exists for CREATE INDEX CONCURRENTLY, which Postgres refuses inside a
// transaction block and which builds an index on a big table without
// blocking writes for the whole build.
//
// A failure part-way leaves whatever already ran in place, and the file is
// rerun from the top on the next boot. A failed or interrupted
// CREATE INDEX CONCURRENTLY also leaves an INVALID index behind, which
// IF NOT EXISTS would then skip. So such a file holds exactly this shape:
// one index, dropped first, then created IF NOT EXISTS under the same name,
// and nothing else (a non-idempotent statement that committed before a crash
// would fail every rerun):
//
//	-- airllm:no-transaction
//	DROP INDEX CONCURRENTLY IF EXISTS idx_usage_ledger_alias_ts;
//	CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_usage_ledger_alias_ts
//	    ON usage_ledger (alias, ts);
//
// TestNoTxMigrationsDropIndexFirst enforces the shape. The DROP also
// rebuilds an index that a previous attempt finished without getting to
// record the file, which is only time lost. See docs/operations.md#migrations.
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
	return s.migrate(ctx, migrations.FS, nil)
}

// MigrateNotify is Migrate that calls onLocked once it holds the migration
// lock, before applying anything; until then it may only be waiting for a
// connection or for another process's migration.
func (s *Store) MigrateNotify(ctx context.Context, onLocked func()) error {
	return s.migrate(ctx, migrations.FS, onLocked)
}

// migrate is Migrate over an arbitrary set of migration files, so tests can
// supply their own without touching the real migrations directory.
func (s *Store) migrate(ctx context.Context, fsys fs.FS, onLocked func()) error {
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
	if onLocked != nil {
		onLocked()
	}

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

// sqlSegKind classifies a piece of SQL source for splitStatements.
type sqlSegKind int

const (
	segCode      sqlSegKind = iota // keywords, names, operators, numbers
	segSpace                       // whitespace
	segComment                     // -- or (nested) /* */ comment
	segQuoted                      // '...', E'...', "...", $tag$...$tag$
	segSemicolon                   // a top-level ;
)

type sqlSeg struct {
	kind sqlSegKind
	text string
}

// lexSQL cuts a SQL script into segments that concatenate back to it. It
// knows just enough of Postgres's lexer to tell a top-level semicolon from
// one inside a string, quoted name, dollar quote or comment.
func lexSQL(script string) []sqlSeg {
	var segs []sqlSeg
	for i := 0; i < len(script); {
		start, c := i, script[i]
		var kind sqlSegKind
		switch {
		case c == ';':
			kind, i = segSemicolon, i+1
		case isSpace(c):
			kind = segSpace
			for i < len(script) && isSpace(script[i]) {
				i++
			}
		case strings.HasPrefix(script[i:], "--"):
			kind = segComment
			if n := strings.IndexByte(script[i:], '\n'); n >= 0 {
				i += n
			} else {
				i = len(script)
			}
		case strings.HasPrefix(script[i:], "/*"):
			// Block comments nest in Postgres.
			kind = segComment
			for depth := 0; i < len(script); {
				if strings.HasPrefix(script[i:], "/*") {
					depth, i = depth+1, i+2
				} else if strings.HasPrefix(script[i:], "*/") {
					depth, i = depth-1, i+2
					if depth == 0 {
						break
					}
				} else {
					i++
				}
			}
		case c == '\'' || c == '"':
			kind, i = segQuoted, quotedEnd(script, i)
		case c == '$' && (i == 0 || !isIdentByte(script[i-1])):
			kind, i = segCode, i+1
			if tag, ok := dollarTag(script[start:]); ok {
				kind = segQuoted
				if n := strings.Index(script[start+len(tag):], tag); n >= 0 {
					i = start + len(tag) + n + len(tag)
				} else {
					i = len(script)
				}
			}
		default:
			kind, i = segCode, i+1
		}
		if n := len(segs); kind == segCode && n > 0 && segs[n-1].kind == segCode {
			segs[n-1].text += script[start:i]
			continue
		}
		segs = append(segs, sqlSeg{kind, script[start:i]})
	}
	return segs
}

// quotedEnd returns the index just past the string or quoted name that
// opens at s[i]. A doubled quote is part of it. An E'...' string also
// escapes with a backslash, and like Postgres, a '...' string continues in
// the next '...' when only whitespace with a newline separates them, which
// keeps E's escapes in effect.
func quotedEnd(s string, i int) int {
	q := s[i]
	escapes := q == '\'' && i > 0 && (s[i-1] == 'E' || s[i-1] == 'e') && (i < 2 || !isIdentByte(s[i-2]))
	for i++; i < len(s); i++ {
		switch {
		case escapes && s[i] == '\\':
			i++
		case s[i] == q:
			if i+1 < len(s) && s[i+1] == q {
				i++
				continue
			}
			if q == '\'' {
				if j, ok := stringContinues(s, i+1); ok {
					i = j
					continue
				}
			}
			return i + 1
		}
	}
	return len(s)
}

// stringContinues reports whether s[j:] is Postgres's quote continuation:
// horizontal whitespace, a newline, then whitespace or -- comments, then a
// quote, whose index it returns.
func stringContinues(s string, j int) (int, bool) {
	for j < len(s) && (s[j] == ' ' || s[j] == '\t' || s[j] == '\f') {
		j++
	}
	if j >= len(s) || (s[j] != '\n' && s[j] != '\r') {
		return 0, false
	}
	for j < len(s) {
		switch {
		case isSpace(s[j]):
			j++
		case strings.HasPrefix(s[j:], "--"):
			n := strings.IndexByte(s[j:], '\n')
			if n < 0 {
				return 0, false
			}
			j += n + 1
		default:
			return j, s[j] == '\''
		}
	}
	return 0, false
}

// splitStatements splits a SQL script into its statements on top-level
// semicolons. Statements that hold nothing but comments and whitespace are
// dropped.
func splitStatements(script string) []string {
	var out []string
	var cur strings.Builder
	hasCode := false
	flush := func() {
		if hasCode {
			out = append(out, strings.TrimSpace(cur.String()))
		}
		cur.Reset()
		hasCode = false
	}
	for _, seg := range lexSQL(script) {
		if seg.kind == segSemicolon {
			flush()
			continue
		}
		cur.WriteString(seg.text)
		if seg.kind == segCode || seg.kind == segQuoted {
			hasCode = true
		}
	}
	flush()
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

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v'
}
