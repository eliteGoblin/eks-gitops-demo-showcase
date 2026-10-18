package postgres_test

import (
	"context"
	"os"
	"testing"

	"github.com/eliteGoblin/eks-gitops-demo/internal/adapter/postgres"
	"github.com/eliteGoblin/eks-gitops-demo/internal/adapter/postgres/pgtest"
)

// The migration Job runs on every deploy, so migrating twice must be a no-op.
func TestMigrateIsIdempotent(t *testing.T) {
	_ = pgtest.NewStore(t) // migrated once in its own schema
	dsn := os.Getenv(pgtest.EnvDSN)
	pool, err := postgres.Open(context.Background(), dsn, postgres.PoolOptions{MaxConns: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	// The default schema may already be migrated (e.g. by `make migrate`); either way Up must succeed twice.
	for range 2 {
		if err := postgres.Migrate(context.Background(), pool, postgres.MigrateOptions{Seed: true}); err != nil {
			t.Fatalf("migrate: %v", err)
		}
	}
}
