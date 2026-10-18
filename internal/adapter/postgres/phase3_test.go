package postgres_test

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/eliteGoblin/eks-gitops-demo/internal/adapter/postgres"
	"github.com/eliteGoblin/eks-gitops-demo/internal/adapter/postgres/pgtest"
	"github.com/eliteGoblin/eks-gitops-demo/internal/adapter/postgres/sqlcdb"
)

func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv(pgtest.EnvDSN)
	if dsn == "" {
		t.Skipf("%s not set", pgtest.EnvDSN)
	}
	return dsn
}

// The pool is bounded and every session carries the timeouts that keep one
// slow transaction from starving a pod (6 pods x the pool must fit RDS).
func TestOpenAppliesPoolSettings(t *testing.T) {
	ctx := context.Background()
	pool, err := postgres.Open(ctx, testDSN(t), postgres.PoolOptions{
		MaxConns: 3, LockTimeout: 2 * time.Second, StatementTimeout: 8 * time.Second, IdleInTransactionTimeout: 15 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	cfg := pool.Config()
	if cfg.MaxConns != 3 || cfg.MaxConnLifetime <= 0 || cfg.MaxConnLifetimeJitter <= 0 {
		t.Fatalf("pool bounds not applied: max=%d lifetime=%v jitter=%v", cfg.MaxConns, cfg.MaxConnLifetime, cfg.MaxConnLifetimeJitter)
	}
	for setting, want := range map[string]string{"lock_timeout": "2s", "statement_timeout": "8s", "idle_in_transaction_session_timeout": "15s", "application_name": "demo-api"} {
		var got string
		if err := pool.QueryRow(ctx, "SELECT current_setting($1)", setting).Scan(&got); err != nil || got != want {
			t.Errorf("%s = %q (%v), want %q", setting, got, err, want)
		}
	}
}

// Without options the pool keeps pgx's defaults apart from the lifetime, so the
// migrate Job is not cut short by the API's statement timeout.
func TestOpenWithoutTimeouts(t *testing.T) {
	ctx := context.Background()
	pool, err := postgres.Open(ctx, testDSN(t), postgres.PoolOptions{MaxConns: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var got string
	if err := pool.QueryRow(ctx, "SELECT current_setting('statement_timeout')").Scan(&got); err != nil || got != "0" {
		t.Fatalf("statement_timeout = %q (%v), want 0", got, err)
	}
}

// `api migrate` creates the application database when it does not exist yet
// (the RDS instance is created with Keycloak's database only).
func TestEnsureDatabaseCreatesIt(t *testing.T) {
	ctx := context.Background()
	base, err := pgxpool.ParseConfig(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	name := "ensure_" + strings.ToLower(time.Now().Format("150405.000000"))
	name = strings.ReplaceAll(name, ".", "_")
	t.Cleanup(func() {
		conn, err := pgx.ConnectConfig(context.Background(), base.ConnConfig)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close(context.Background()) }()
		_, _ = conn.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name)
	})
	u, err := url.Parse(testDSN(t))
	if err != nil || u.Scheme == "" {
		t.Skip("needs a URL-form TEST_DATABASE_URL")
	}
	u.Path = "/" + name
	dsn := u.String()
	if _, err := postgres.Open(ctx, dsn, postgres.PoolOptions{MaxConns: 1}); err == nil {
		t.Fatalf("database %s must not exist before the test", name)
	}
	for range 2 { // idempotent
		if err := postgres.EnsureDatabase(ctx, dsn); err != nil {
			t.Fatalf("ensure database: %v", err)
		}
	}
	pool, err := postgres.Open(ctx, dsn, postgres.PoolOptions{MaxConns: 1})
	if err != nil {
		t.Fatalf("database must exist afterwards: %v", err)
	}
	pool.Close()
}

// The demo seed is an explicit choice, never a side effect of migrating.
func TestSeedIsExplicit(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewSchemaPool(t)
	count := func() int {
		var n int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM participants").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if err := postgres.Migrate(ctx, pool, postgres.MigrateOptions{}); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 0 {
		t.Fatalf("schema-only migration seeded %d participants", n)
	}
	for range 2 { // seeding twice is a no-op
		if err := postgres.Migrate(ctx, pool, postgres.MigrateOptions{Seed: true}); err != nil {
			t.Fatal(err)
		}
	}
	if n := count(); n != 3 {
		t.Fatalf("seed loaded %d participants, want 3", n)
	}
}

// A panic inside a transaction must not leak the plan lock (and its pool
// connection): the deferred rollback releases it.
func TestPanicInTransactionReleasesTheLock(t *testing.T) {
	st := pgtest.NewStore(t)
	ctx := context.Background()
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("the panic must propagate")
			}
		}()
		_ = st.InTx(ctx, func(q *sqlcdb.Queries) error {
			if _, err := q.LockPlan(ctx, "PL1"); err != nil {
				return err
			}
			panic("boom")
		})
	}()
	if err := st.InTxRaw(ctx, "SELECT id FROM plans WHERE id = 'PL1' FOR UPDATE NOWAIT"); err != nil {
		t.Fatalf("the plan lock leaked: %v", err)
	}
}

// Locking a plan for a budget write must not block inserts that merely
// reference it (a participant raising a support request meanwhile).
func TestPlanLockAllowsReferencingInserts(t *testing.T) {
	st := pgtest.NewStore(t)
	ctx := context.Background()
	release, err := st.HoldPlanLock(ctx, "PL1")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	err = st.InTxRaw(ctx, `SET LOCAL lock_timeout = '1s';
		INSERT INTO support_requests (id, plan_id, participant_id, requested_by, requested_by_name, category, description, estimated_cents, status, raised_at)
		VALUES ('SR-lock', 'PL1', 'P1', 'a', 'a', 'THERAPY', 'x', 100, 'PENDING', now())`)
	if err != nil {
		t.Fatalf("an insert referencing the locked plan must not wait on the budget lock: %v", err)
	}
}
