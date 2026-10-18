package postgres_test

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/eliteGoblin/eks-gitops-demo/internal/adapter/postgres"
	"github.com/eliteGoblin/eks-gitops-demo/internal/adapter/postgres/pgtest"
	"github.com/eliteGoblin/eks-gitops-demo/internal/app"
	"github.com/eliteGoblin/eks-gitops-demo/internal/domain"
)

// A claim that cannot get the plan's budget lock in time is "try again"
// (app.ErrUnavailable), not an internal error.
func TestLockTimeoutIsUnavailable(t *testing.T) {
	st := pgtest.NewStore(t, pgtest.WithRuntimeParam("lock_timeout", "200ms"))
	ctx := context.Background()
	release, err := st.HoldPlanLock(ctx, "PL1")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	c := domain.Claim{ID: "C-locked", PlanID: "PL1", ProviderID: "PR1", Category: domain.CategoryDailyLiving, Amount: 1_00,
		Status: domain.ClaimSubmitted, ServiceDate: "2026-05-01", SubmittedAt: time.Now()}
	err = st.InsertClaimWithinBudget(ctx, c, func([]domain.Claim, []domain.ServiceBooking) error { return nil })
	if !errors.Is(err, app.ErrUnavailable) {
		t.Fatalf("want app.ErrUnavailable, got %v", err)
	}
}

// Budget writes rely on READ COMMITTED (a read after the plan lock sees what
// the previous holder committed), so the level is pinned and a different
// database default cannot change it.
func TestWriteTransactionsAreReadCommitted(t *testing.T) {
	st := pgtest.NewStore(t, pgtest.WithRuntimeParam("default_transaction_isolation", "repeatable read"))
	level, err := st.WriteTxIsolation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if level != "read committed" {
		t.Fatalf("write transactions run at %q, want read committed", level)
	}
}

// The readiness check answers from a saturated pool without queueing behind
// requests: busy connections mean the database is serving queries.
func TestReachableDoesNotWaitForPoolCapacity(t *testing.T) {
	st := pgtest.NewStore(t, pgtest.WithMaxConns(1))
	ctx := context.Background()
	release, err := st.HoldPlanLock(ctx, "PL1") // takes the only connection
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	probe, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	start := time.Now()
	if err := st.Reachable(probe); err != nil {
		t.Fatalf("a saturated pool is serving queries: %v", err)
	}
	if waited := time.Since(start); waited > 200*time.Millisecond {
		t.Fatalf("the check waited %v for a connection", waited)
	}
}

func TestReachablePingsWhenThePoolHasRoom(t *testing.T) {
	if err := pgtest.NewStore(t).Reachable(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestReachableFailsWithoutADatabase(t *testing.T) {
	pool := pgtest.NewSchemaPool(t)
	st := postgres.New(pool)
	pool.Close()
	if err := st.Reachable(context.Background()); err == nil {
		t.Fatal("a closed pool cannot reach the database")
	}
}

// Several migrate Jobs creating the same database at once all succeed.
func TestEnsureDatabaseToleratesConcurrentCallers(t *testing.T) {
	ctx := context.Background()
	u, err := url.Parse(testDSN(t))
	if err != nil || u.Scheme == "" {
		t.Skip("needs a URL-form TEST_DATABASE_URL")
	}
	name := "race_" + strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "_")
	t.Cleanup(func() { dropDatabase(t, name) })
	u.Path = "/" + name
	errs := make([]error, 5)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Go(func() { errs[i] = postgres.EnsureDatabase(ctx, u.String()) })
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("caller %d: %v", i, err)
		}
	}
	pool, err := postgres.Open(ctx, u.String(), postgres.PoolOptions{MaxConns: 1})
	if err != nil {
		t.Fatalf("the database must exist: %v", err)
	}
	pool.Close()
}

func dropDatabase(t *testing.T, name string) {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(testDSN(t))
	if err != nil {
		return
	}
	conn, err := pgx.ConnectConfig(context.Background(), cfg.ConnConfig)
	if err != nil {
		return
	}
	defer func() { _ = conn.Close(context.Background()) }()
	_, _ = conn.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
}
