package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/eliteGoblin/eks-gitops-demo/internal/adapter/memory"
	"github.com/eliteGoblin/eks-gitops-demo/internal/adapter/postgres"
	"github.com/eliteGoblin/eks-gitops-demo/internal/app"
	"github.com/eliteGoblin/eks-gitops-demo/internal/platform"
	"github.com/eliteGoblin/eks-gitops-demo/internal/platform/otel/otelpgx"
)

// repositories is every port a store adapter implements.
type repositories interface {
	app.ParticipantRepository
	app.PlanRepository
	app.ProviderRepository
	app.ClaimRepository
	app.BookingRepository
	app.SupportRequestRepository
	app.Relationships
}

// Session limits for the API's pool: a statement never outlives the request,
// a lock wait is short, and an idle transaction is cut off; see postgres.PoolOptions.
const (
	dbLockTimeout      = 2 * time.Second
	dbStatementTimeout = requestTimeout - 2*time.Second
	dbIdleInTxTimeout  = 15 * time.Second
)

// openStore returns the configured store, a check that the database is
// reachable (main runs it in the background for readiness, see
// platform.CachedCheck) and a close function. The memory store is seeded per
// process; Postgres is migrated by `api migrate` (the chart runs it as a
// pre-upgrade Job) and seeded only with `api migrate --seed`.
func openStore(ctx context.Context, cfg config) (repositories, platform.Check, func(), error) {
	if cfg.Store == storeMemory {
		st := memory.New()
		memory.Seed(st)
		return st, func(context.Context) error { return nil }, func() {}, nil
	}
	pool, err := postgres.Open(ctx, cfg.DatabaseURL, postgres.PoolOptions{
		MaxConns:                 int32(cfg.DBMaxConns), //nolint:gosec // validated >= 1 and small
		LockTimeout:              dbLockTimeout,
		StatementTimeout:         dbStatementTimeout,
		IdleInTransactionTimeout: dbIdleInTxTimeout,
		Tracer:                   otelpgx.NewTracer(),
	})
	if err != nil {
		return nil, nil, nil, err
	}
	if err := otelpgx.RecordPoolStats(pool); err != nil {
		pool.Close()
		return nil, nil, nil, fmt.Errorf("pool metrics: %w", err)
	}
	st := postgres.New(pool)
	return st, st.Reachable, pool.Close, nil
}

// migrateOptions are the flags of `api migrate`.
type migrateOptions struct {
	Seed           bool // --seed: load the demo data set
	CreateDatabase bool // --create-db: create the database first if it is missing (dev only)
}

// migrateFlags parses the arguments of `api migrate`.
func migrateFlags(args []string) (migrateOptions, error) {
	var opts migrateOptions
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.BoolVar(&opts.Seed, "seed", false, "load the demo data set")
	fs.BoolVar(&opts.CreateDatabase, "create-db", false, "create the database if it is missing")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		return migrateOptions{}, fmt.Errorf("usage: api migrate [--seed] [--create-db], got %q", args)
	}
	return opts, nil
}

// migrate applies the migrations (and the demo seed when asked) and exits;
// used by the migration Job. It creates a missing database only when asked:
// in an environment where the database is provisioned, a mistyped name in
// DATABASE_URL must fail the Job, not create an empty database. Its pool has
// no statement timeout: a migration may legitimately run longer than a request.
func migrate(ctx context.Context, cfg config, opts migrateOptions) error {
	if cfg.Store != storePostgres {
		return fmt.Errorf("migrate needs STORE=%s", storePostgres)
	}
	if opts.CreateDatabase {
		if err := postgres.EnsureDatabase(ctx, cfg.DatabaseURL); err != nil {
			return err
		}
	}
	pool, err := postgres.Open(ctx, cfg.DatabaseURL, postgres.PoolOptions{MaxConns: 2})
	if err != nil {
		return err
	}
	defer pool.Close()
	return postgres.Migrate(ctx, pool, postgres.MigrateOptions{Seed: opts.Seed})
}
