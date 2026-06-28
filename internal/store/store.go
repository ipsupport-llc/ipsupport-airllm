// Package store wires the durable (Postgres) and ephemeral (Redis)
// backends and applies schema migrations.
package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/rromenskyi/ipsupport-airllm/internal/config"
)

// Store holds the shared connection pools.
type Store struct {
	PG  *pgxpool.Pool
	RDB *redis.Client
}

// Open connects to Postgres and Redis, pinging both before returning.
func Open(ctx context.Context, cfg *config.Config) (*Store, error) {
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("postgres connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres ping: %w", err)
	}

	opt, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("redis url: %w", err)
	}
	rdb := redis.NewClient(opt)
	if err := rdb.Ping(ctx).Err(); err != nil {
		pool.Close()
		_ = rdb.Close()
		return nil, fmt.Errorf("redis ping: %w", err)
	}

	return &Store{PG: pool, RDB: rdb}, nil
}

// ProviderRow is an enabled provider with the fields needed to build a client.
type ProviderRow struct {
	Name           string
	Kind           string
	BaseURL        string
	CredEnc        []byte // AES-GCM sealed API key (nil if none)
	MaxConcurrency int
}

// ListProvidersForRegistry returns all enabled providers for the registry.
func (s *Store) ListProvidersForRegistry(ctx context.Context) ([]ProviderRow, error) {
	rows, err := s.PG.Query(ctx,
		`SELECT name, kind, base_url, cred_enc, max_concurrency FROM providers WHERE enabled = true ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProviderRow
	for rows.Next() {
		var p ProviderRow
		if err := rows.Scan(&p.Name, &p.Kind, &p.BaseURL, &p.CredEnc, &p.MaxConcurrency); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Close releases both backends.
func (s *Store) Close() {
	if s.PG != nil {
		s.PG.Close()
	}
	if s.RDB != nil {
		_ = s.RDB.Close()
	}
}
