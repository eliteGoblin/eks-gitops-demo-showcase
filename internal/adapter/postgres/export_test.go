package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/eliteGoblin/eks-gitops-demo/internal/adapter/postgres/sqlcdb"
)

// InTxRaw runs raw SQL in one read-write transaction through the same
// commit/rollback handling as the store.
func (s *Store) InTxRaw(ctx context.Context, sql string) error {
	return s.withTx(ctx, writeTx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, sql)
		return err
	})
}

// WriteTxIsolation reports the isolation level the store's write
// transactions run at.
func (s *Store) WriteTxIsolation(ctx context.Context) (string, error) {
	var level string
	err := s.withTx(ctx, writeTx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SHOW transaction_isolation").Scan(&level)
	})
	return level, err
}

// InReadTx exposes the read-only transaction helper to the external tests.
func (s *Store) InReadTx(ctx context.Context, fn func(q *sqlcdb.Queries) error) error {
	return s.inReadTx(ctx, fn)
}

// Exec runs raw SQL for test setup.
func (s *Store) Exec(ctx context.Context, sql string) error {
	_, err := s.pool.Exec(ctx, sql)
	return err
}

// InTx exposes the read-write transaction helper to the external tests.
func (s *Store) InTx(ctx context.Context, fn func(q *sqlcdb.Queries) error) error {
	return s.inTx(ctx, fn)
}

// HoldPlanLock takes the budget lock on a plan in a transaction of its own
// and keeps it until release is called.
func (s *Store) HoldPlanLock(ctx context.Context, planID string) (release func(), err error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, err
	}
	if _, err := s.q.WithTx(tx).LockPlan(ctx, planID); err != nil {
		_ = tx.Rollback(ctx)
		return nil, err
	}
	return func() { _ = tx.Rollback(context.Background()) }, nil
}

// Pool exposes the store's pool to the external tests (to hold a competing
// transaction open).
func (s *Store) Pool() *pgxpool.Pool { return s.pool }
