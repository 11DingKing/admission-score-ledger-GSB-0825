// Package testdb provides an isolated PostgreSQL database for integration
// tests. It creates a uniquely named database from DATABASE_URL (or a local
// default), runs migrations and drops it on cleanup.
package testdb

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"admission-score-ledger/internal/repository"
)

// Execer is satisfied by pgx.Tx and *pgxpool.Pool.
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

const defaultURL = "postgres:///postgres?sslmode=disable"

// New creates a fresh migrated database and returns a pool connected to it.
// Tests are skipped when no PostgreSQL server is reachable.
func New(t *testing.T) *pgxpool.Pool {
	t.Helper()

	adminURL := os.Getenv("TEST_DATABASE_URL")
	if adminURL == "" {
		adminURL = os.Getenv("DATABASE_URL")
	}
	if adminURL == "" {
		adminURL = defaultURL
	}

	adminCfg, err := pgxpool.ParseConfig(adminURL)
	if err != nil {
		t.Fatalf("parse DATABASE_URL: %v", err)
	}

	connectCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	adminPool, err := pgxpool.NewWithConfig(connectCtx, adminCfg)
	if err != nil {
		t.Skipf("skip integration test: cannot connect to PostgreSQL: %v", err)
	}
	if err := adminPool.Ping(connectCtx); err != nil {
		adminPool.Close()
		t.Skipf("skip integration test: PostgreSQL not reachable at %q: %v", redacted(adminURL), err)
	}

	dbName := fmt.Sprintf("asl_test_%d", time.Now().UnixNano())
	if _, err := adminPool.Exec(context.Background(),
		fmt.Sprintf(`CREATE DATABASE %s`, dbName)); err != nil {
		adminPool.Close()
		t.Fatalf("create test database: %v", err)
	}
	t.Cleanup(func() {
		adminPool.Exec(context.Background(), fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, dbName))
		adminPool.Close()
	})

	targetURL := databaseURL(adminURL, dbName)
	cfg, err := pgxpool.ParseConfig(targetURL)
	if err != nil {
		t.Fatalf("parse test database url: %v", err)
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	t.Cleanup(pool.Close)

	migrateCtx, migrateCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer migrateCancel()
	tx, err := pool.Begin(migrateCtx)
	if err != nil {
		t.Fatalf("begin migration tx: %v", err)
	}
	if err := repository.Migrate(migrateCtx, tx); err != nil {
		_ = tx.Rollback(migrateCtx)
		t.Fatalf("run migrations: %v", err)
	}
	if err := tx.Commit(migrateCtx); err != nil {
		t.Fatalf("commit migrations: %v", err)
	}
	return pool
}

func databaseURL(adminURL, dbName string) string {
	if strings.HasPrefix(adminURL, "postgres://") || strings.HasPrefix(adminURL, "postgresql://") {
		u, err := url.Parse(adminURL)
		if err == nil {
			u.Path = "/" + dbName
			return u.String()
		}
	}
	return adminURL
}

func redacted(s string) string {
	if u, err := url.Parse(s); err == nil && u != nil {
		u.RawQuery = ""
		return u.String()
	}
	return s
}

// CountRows returns the number of rows in table matching the optional
// natural-key predicate. It is a small helper for integration assertions.
func CountRows(t *testing.T, pool *pgxpool.Pool, table string, predicate string, args ...any) int {
	t.Helper()
	query := "SELECT count(*) FROM " + table
	if predicate != "" {
		query += " WHERE " + predicate
	}
	var n int
	if err := pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// MustExec runs a statement that the test expects to succeed.
func MustExec(t *testing.T, e Execer, sql string, args ...any) {
	t.Helper()
	if _, err := e.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}
