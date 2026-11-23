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
	"time"

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
	// lockConn holds the shared advisory lock for the life of the test
	// binary. It must stay reachable: an unreferenced connection is closed by
	// the socket's finalizer on the next GC, which would release the lock.
	lockConn *pgx.Conn
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
	// A black-holed server fails the run in a minute instead of hanging
	// until go test's 10m timeout.
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	name, err := dbName()
	if err != nil {
		return "", err
	}
	d, err := dsnFor(admin, name)
	if err != nil {
		return "", err
	}
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		return "", fmt.Errorf("connect to TEST_DATABASE_URL: %w", err)
	}
	// Two `go test` runs of the same package in one checkout share the
	// database name. Setup holds an exclusive advisory lock; afterwards this
	// session keeps a shared one until the test binary exits (the connection
	// is deliberately never closed), so another run's DROP waits for this
	// run to finish instead of killing it.
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(hashtext($1))`, name); err != nil {
		conn.Close(context.Background())
		return "", fmt.Errorf("lock %s: %w", name, err)
	}
	ident := pgx.Identifier{name}.Sanitize()
	if _, err := conn.Exec(ctx, `DROP DATABASE IF EXISTS `+ident+` WITH (FORCE)`); err != nil {
		conn.Close(context.Background())
		return "", fmt.Errorf("drop old %s: %w", name, err)
	}
	if _, err := conn.Exec(ctx, `CREATE DATABASE `+ident); err != nil {
		conn.Close(context.Background())
		return "", fmt.Errorf("create %s: %w", name, err)
	}
	defer func() {
		// Downgrade: take the shared lock first, then drop the exclusive one.
		if _, err := conn.Exec(context.Background(), `SELECT pg_advisory_lock_shared(hashtext($1)), pg_advisory_unlock(hashtext($1))`, name); err != nil {
			conn.Close(context.Background())
			return
		}
		lockConn = conn
	}()

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

// dsnFor points admin at database name. pgx lets ?dbname= / ?database=
// override the path, so both are removed: a URL carrying one must not send
// the migration and the tests to that database.
func dsnFor(admin, name string) (string, error) {
	u, err := url.Parse(admin)
	if err != nil {
		return "", fmt.Errorf("parse TEST_DATABASE_URL: %w", err)
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return "", fmt.Errorf("TEST_DATABASE_URL must be a postgres:// URL, not a keyword DSN")
	}
	u.Path = "/" + name
	u.RawPath = ""
	q := u.Query()
	q.Del("dbname")
	q.Del("database")
	u.RawQuery = q.Encode()
	d := u.String()
	cfg, err := pgx.ParseConfig(d)
	if err != nil {
		return "", fmt.Errorf("parse test DSN: %w", err)
	}
	if cfg.Database != name {
		return "", fmt.Errorf("test DSN resolves to database %q, want %q", cfg.Database, name)
	}
	return d, nil
}
