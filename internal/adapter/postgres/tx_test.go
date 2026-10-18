package postgres_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/eliteGoblin/eks-gitops-demo/internal/adapter/postgres"
	"github.com/eliteGoblin/eks-gitops-demo/internal/adapter/postgres/pgtest"
	"github.com/eliteGoblin/eks-gitops-demo/internal/adapter/postgres/sqlcdb"
)

// A failure at COMMIT (here a deferred constraint) must reach the caller,
// never be mistaken for success.
func TestCommitFailureIsReturned(t *testing.T) {
	st := pgtest.NewStore(t)
	ctx := context.Background()
	if err := st.Exec(ctx, `CREATE TABLE parent (id int PRIMARY KEY);
		CREATE TABLE child (parent_id int REFERENCES parent (id) DEFERRABLE INITIALLY DEFERRED)`); err != nil {
		t.Fatal(err)
	}
	if err := st.InTxRaw(ctx, "INSERT INTO parent VALUES (1); INSERT INTO child VALUES (1)"); err != nil {
		t.Fatalf("valid transaction: %v", err)
	}
	err := st.InTxRaw(ctx, "INSERT INTO child VALUES (42)") // violates the deferred FK only at COMMIT
	if err == nil || !strings.Contains(err.Error(), "commit") {
		t.Fatalf("a deferred violation must fail the commit and be returned, got %v", err)
	}
}

func TestReadTransactionsAreReadOnly(t *testing.T) {
	st := pgtest.NewStore(t)
	ctx := context.Background()
	err := st.InReadTx(ctx, func(q *sqlcdb.Queries) error {
		_, err := q.UpdateClaim(ctx, sqlcdb.UpdateClaimParams{ID: "C1", Status: "PAID", FromStatus: "APPROVED"})
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("a write inside a read transaction must fail, got %v", err)
	}
	if err := st.InReadTx(ctx, func(q *sqlcdb.Queries) error { return errors.New("boom") }); err == nil || err.Error() != "boom" {
		t.Fatalf("the function's error must come back unchanged, got %v", err)
	}
}

// Connection errors must never echo the connection string: it carries the password.
func TestOpenNeverLeaksThePassword(t *testing.T) {
	ctx := context.Background()
	for _, dsn := range []string{
		"postgres://demo:s3cret-PW@127.0.0.1:1/demo?sslmode=disable&connect_timeout=1", // nothing listening
		"postgres://demo:s3cret-PW@%zz/demo",                                           // unparsable
	} {
		_, err := postgres.Open(ctx, dsn, postgres.PoolOptions{})
		if err == nil {
			t.Fatalf("%q: want an error", dsn)
		}
		if strings.Contains(err.Error(), "s3cret-PW") {
			t.Fatalf("error leaks the password: %v", err)
		}
	}
}
