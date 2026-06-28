// Package store wires the durable (Postgres) and ephemeral (Redis)
// backends and applies schema migrations.
package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/rromenskyi/ipsupport-airouter/internal/config"
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

// Close releases both backends.
func (s *Store) Close() {
	if s.PG != nil {
		s.PG.Close()
	}
	if s.RDB != nil {
		_ = s.RDB.Close()
	}
}
