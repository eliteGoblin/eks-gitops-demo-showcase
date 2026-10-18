// Package pgtest gives tests a migrated, seeded Postgres store in a schema of
// their own, so contract tests run in isolation (and in parallel) against one
// server. Tests that need it are skipped unless TEST_DATABASE_URL is set.
package pgtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/eliteGoblin/eks-gitops-demo/internal/adapter/postgres"
)

// EnvDSN names the variable holding the test server's connection string.
const EnvDSN = "TEST_DATABASE_URL"

// Option tunes the pool a store is returned on (applied after migrating, so
// a small pool or a short timeout cannot get in the migrations' way).
type Option func(cfg *pgxpool.Config)

// WithMaxConns caps the store's pool.
func WithMaxConns(n int32) Option { return func(cfg *pgxpool.Config) { cfg.MaxConns = n } }

// WithRuntimeParam sets a session parameter (e.g. lock_timeout) on every connection.
func WithRuntimeParam(name, value string) Option {
	return func(cfg *pgxpool.Config) { cfg.ConnConfig.RuntimeParams[name] = value }
}

// NewStore creates a fresh schema, migrates and seeds it, and drops it when
// the test ends.
func NewStore(t testing.TB, opts ...Option) *postgres.Store {
	t.Helper()
	pool := NewSchemaPool(t)
	if err := postgres.Migrate(context.Background(), pool, postgres.MigrateOptions{Seed: true}); err != nil {
		t.Fatalf("pgtest: %v", err)
	}
	if len(opts) == 0 {
		return postgres.New(pool)
	}
	cfg := pool.Config() // a copy: same schema, tuned
	for _, opt := range opts {
		opt(cfg)
	}
	tuned, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("pgtest: %v", err)
	}
	t.Cleanup(tuned.Close) // runs before the schema is dropped (cleanups run last-in, first-out)
	return postgres.New(tuned)
}

// NewSchemaPool returns a pool whose sessions use a fresh, empty schema that
// is dropped when the test ends. Nothing is migrated.
func NewSchemaPool(t testing.TB) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv(EnvDSN)
	if dsn == "" {
		t.Skipf("%s not set", EnvDSN)
	}
	ctx := context.Background()
	schema := "test_" + randomSuffix(t)

	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("pgtest: connect: %v", err)
	}
	defer func() { _ = admin.Close(ctx) }()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("pgtest: create schema: %v", err)
	}

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("pgtest: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pgtest: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		conn, err := pgx.Connect(context.Background(), dsn)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close(context.Background()) }()
		_, _ = conn.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
	})
	return pool
}

func randomSuffix(t testing.TB) string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}
