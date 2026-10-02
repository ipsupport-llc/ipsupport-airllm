package store

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/ipsupport-llc/ipsupport-airllm/migrations"
)

// migrationLockKey is a fixed, arbitrary key for the session-level advisory
// lock Migrate holds for its duration. Any constant works as long as every
// process uses the same one.
const migrationLockKey = 72_70_01

// Migrate applies any embedded *.sql migrations not yet recorded, in
// lexicographic order, each inside its own transaction.
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

	entries, err := migrations.FS.ReadDir(".")
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

		sqlBytes, err := migrations.FS.ReadFile(f)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", f, err)
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
