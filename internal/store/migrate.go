package store

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/rromenskyi/ipsupport-airouter/migrations"
)

// Migrate applies any embedded *.sql migrations not yet recorded, in
// lexicographic order, each inside its own transaction.
func (s *Store) Migrate(ctx context.Context) error {
	if _, err := s.PG.Exec(ctx,
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
		if err := s.PG.QueryRow(ctx,
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

		tx, err := s.PG.Begin(ctx)
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
