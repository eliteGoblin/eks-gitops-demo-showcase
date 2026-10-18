package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/eliteGoblin/eks-gitops-demo/internal/adapter/postgres/sqlcdb"
	"github.com/eliteGoblin/eks-gitops-demo/internal/app"
	"github.com/eliteGoblin/eks-gitops-demo/internal/domain"
)

// InsertClaimOnce runs InsertClaimWithinBudget's transaction (plan row lock,
// budget check, insert) and records the idempotency key in it. A retry on the
// same plan waits for the plan lock and then finds the key. A request with
// the same key on another plan holds another lock: its key insert waits on
// the primary key and fails once the first commits, rolling its claim back;
// the key is then read again and reported as used.
func (s *Store) InsertClaimOnce(ctx context.Context, c domain.Claim, k app.IdempotencyKey, check app.BudgetCheck) (app.IdempotencyRecord, bool, error) {
	var prior app.IdempotencyRecord
	var replayed bool
	err := s.inTx(ctx, func(q *sqlcdb.Queries) error {
		if _, err := q.LockPlan(ctx, c.PlanID); err != nil {
			return notFound(err)
		}
		rec, found, err := idempotencyRecord(ctx, q, k)
		if err != nil || found {
			prior, replayed = rec, found
			return err
		}
		claims, err := q.ClaimsOfPlan(ctx, c.PlanID)
		if err != nil {
			return err
		}
		bookings, err := q.BookingsOfPlan(ctx, c.PlanID)
		if err != nil {
			return err
		}
		if err := check(mapRows(claims, toClaim), mapRows(bookings, toBooking)); err != nil {
			return err
		}
		if err := q.InsertClaim(ctx, insertClaimParams(c)); err != nil {
			return err
		}
		return q.InsertIdempotencyKey(ctx, sqlcdb.InsertIdempotencyKeyParams{
			Subject: k.Subject, Key: k.Key, Fingerprint: k.Fingerprint, ClaimID: c.ID,
		})
	})
	if isIdempotencyConflict(err) {
		rec, found, rerr := idempotencyRecord(ctx, s.q, k)
		if rerr != nil {
			return app.IdempotencyRecord{}, false, rerr
		}
		if found {
			return rec, true, nil
		}
	}
	if err != nil {
		return app.IdempotencyRecord{}, false, err
	}
	return prior, replayed, nil
}

// idempotencyRecord reads what the caller's key points at, if anything.
func idempotencyRecord(ctx context.Context, q *sqlcdb.Queries, k app.IdempotencyKey) (app.IdempotencyRecord, bool, error) {
	row, err := q.GetIdempotencyKey(ctx, sqlcdb.GetIdempotencyKeyParams{Subject: k.Subject, Key: k.Key})
	if errors.Is(err, pgx.ErrNoRows) {
		return app.IdempotencyRecord{}, false, nil
	}
	if err != nil {
		return app.IdempotencyRecord{}, false, err
	}
	return app.IdempotencyRecord{ClaimID: row.ClaimID, Fingerprint: row.Fingerprint}, true, nil
}

// isIdempotencyConflict reports a unique violation on the idempotency key.
func isIdempotencyConflict(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "idempotency_keys_pkey"
}
