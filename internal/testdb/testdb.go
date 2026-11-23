// Package testdb gives Postgres-backed integration tests a private,
// freshly migrated database per test binary.
//
// `go test ./...` builds one binary per package and runs them in parallel,
// so tests in different packages pointed at the same TEST_DATABASE_URL
// would otherwise see (and trip over) each other's rows. Pool creates a
// database named after the package directory on first use, drops any
// leftover copy from an earlier run first, and applies the schema through
// the caller's migrate function, so every package starts from an empty,
// current schema. The TEST_DATABASE_URL role therefore needs CREATEDB (the
// POSTGRES_USER of the official image, as in the dev compose stack and CI,
// has it).
//
// Only test code imports this package.
package testdb

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// MigrateFunc brings an empty database to the current schema. Callers pass
// the real migrator (store.Migrate) so tests run against exactly what a
// deployment applies; testdb cannot call it itself because the store
// package's own tests import testdb.
type MigrateFunc func(context.Context, *pgxpool.Pool) error

var (
	once   sync.Once
	dsn    string
	setErr error
)

// Pool returns a pool on this test binary's private database, creating and
// migrating it on first use. Skips the test when TEST_DATABASE_URL is unset.
// The pool is closed when the test ends.
func Pool(t testing.TB, migrate MigrateFunc) *pgxpool.Pool {
	t.Helper()
	d := DSN(t, migrate)
	pool, err := pgxpool.New(context.Background(), d)
	if err != nil {
		t.Fatalf("connect to test database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// DSN is Pool's connection string, for tests that build their own pool or
// route the connection through a proxy.
func DSN(t testing.TB, migrate MigrateFunc) string {
	t.Helper()
	admin := os.Getenv("TEST_DATABASE_URL")
	if admin == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping Postgres integration test")
	}
	once.Do(func() { dsn, setErr = setup(admin, migrate) })
	if setErr != nil {
		t.Fatalf("set up test database: %v", setErr)
	}
	return dsn
}

var unsafeChars = regexp.MustCompile(`[^a-z0-9]+`)

// dbName is stable per package directory: a rerun replaces its own
// database instead of leaving a new one behind, and two checkouts of the
// repo (different absolute paths) never share one.
func dbName() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(wd))
	base := unsafeChars.ReplaceAllString(strings.ToLower(filepath.Base(wd)), "_")
	if len(base) > 30 {
		base = base[:30]
	}
	return "airllm_test_" + base + "_" + hex.EncodeToString(sum[:4]), nil
}

func setup(admin string, migrate MigrateFunc) (string, error) {
	ctx := context.Background()
	name, err := dbName()
	if err != nil {
		return "", err
	}
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		return "", fmt.Errorf("connect to TEST_DATABASE_URL: %w", err)
	}
	defer conn.Close(ctx)
	ident := pgx.Identifier{name}.Sanitize()
	if _, err := conn.Exec(ctx, `DROP DATABASE IF EXISTS `+ident+` WITH (FORCE)`); err != nil {
		return "", fmt.Errorf("drop old %s: %w", name, err)
	}
	if _, err := conn.Exec(ctx, `CREATE DATABASE `+ident); err != nil {
		return "", fmt.Errorf("create %s: %w", name, err)
	}

	u, err := url.Parse(admin)
	if err != nil {
		return "", fmt.Errorf("parse TEST_DATABASE_URL: %w", err)
	}
	u.Path = "/" + name
	d := u.String()

	pool, err := pgxpool.New(ctx, d)
	if err != nil {
		return "", fmt.Errorf("connect to %s: %w", name, err)
	}
	defer pool.Close()
	if err := migrate(ctx, pool); err != nil {
		return "", fmt.Errorf("migrate %s: %w", name, err)
	}
	return d, nil
}
