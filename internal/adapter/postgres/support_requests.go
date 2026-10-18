package postgres

import (
	"context"

	"github.com/eliteGoblin/eks-gitops-demo/internal/adapter/postgres/sqlcdb"
	"github.com/eliteGoblin/eks-gitops-demo/internal/app"
	"github.com/eliteGoblin/eks-gitops-demo/internal/domain"
)

func (s *Store) InsertSupportRequest(ctx context.Context, r domain.SupportRequest) error {
	return s.q.InsertSupportRequest(ctx, sqlcdb.InsertSupportRequestParams{
		ID: r.ID, PlanID: r.PlanID, ParticipantID: r.ParticipantID, RequestedBy: r.RequestedBy,
		RequestedByName: r.RequestedByName, Category: string(r.Category), Description: r.Description,
		EstimatedCents: int64(r.EstimatedAmount), PreferredProviderID: optional(r.PreferredProviderID),
		Status: string(r.Status), RaisedAt: r.RaisedAt,
	})
}

func (s *Store) GetSupportRequest(ctx context.Context, id string) (domain.SupportRequest, error) {
	r, err := s.q.GetSupportRequest(ctx, id)
	if err != nil {
		return domain.SupportRequest{}, notFound(err)
	}
	return toSupportRequest(r), nil
}

func (s *Store) GetSupportRequests(ctx context.Context, ids []string) ([]domain.SupportRequest, error) {
	rows, err := s.q.GetSupportRequests(ctx, ids)
	return mapRows(rows, toSupportRequest), err
}

func (s *Store) ListSupportRequests(ctx context.Context, q app.SupportRequestQuery) ([]domain.SupportRequest, error) {
	var status *string
	if q.Status != nil {
		st := string(*q.Status)
		status = &st
	}
	rows, err := s.q.ListSupportRequests(ctx, sqlcdb.ListSupportRequestsParams{
		AllScope: q.Scope.All, RequestedBy: q.Scope.RequestedBy, ParticipantIds: nonNil(q.Scope.ParticipantIDs), Status: status,
	})
	return mapRows(rows, toSupportRequest), err
}

// DecideSupportRequest stores the decision, and for an approval the
// reservation, in one transaction. The plan row is locked first so that the
// budget check sees every committed reservation; the conditional UPDATE then
// guarantees only one decision per request wins.
func (s *Store) DecideSupportRequest(ctx context.Context, r domain.SupportRequest, reserve app.Reservation) error {
	return s.inTx(ctx, func(q *sqlcdb.Queries) error {
		if _, err := q.LockPlan(ctx, r.PlanID); err != nil {
			return notFound(err)
		}
		if err := s.checkPending(ctx, q, r.ID); err != nil {
			return err
		}
		if r.Status == domain.SupportRequestApproved {
			if err := saveReservation(ctx, q, r.PlanID, reserve); err != nil {
				return err
			}
		}
		n, err := q.DecideSupportRequest(ctx, sqlcdb.DecideSupportRequestParams{
			ID: r.ID, Status: string(r.Status), DecidedBy: optional(r.DecidedBy), DecisionReason: optional(r.DecisionReason),
			BookedProviderID: optional(r.BookedProviderID), DecidedAt: r.DecidedAt,
		})
		if err != nil {
			return err
		}
		if n == 0 {
			return alreadyDecided()
		}
		return nil
	})
}

func (s *Store) checkPending(ctx context.Context, q *sqlcdb.Queries, id string) error {
	status, err := q.SupportRequestStatus(ctx, id)
	if err != nil {
		return notFound(err)
	}
	if status != string(domain.SupportRequestPending) {
		return alreadyDecided()
	}
	return nil
}

func alreadyDecided() error {
	return &domain.ValidationError{Field: "id", Message: "support request is already decided"}
}

// saveReservation reads the locked plan's claims and bookings, lets reserve
// decide the provider's booking and stores it.
func saveReservation(ctx context.Context, q *sqlcdb.Queries, planID string, reserve app.Reservation) error {
	claims, err := q.ClaimsOfPlan(ctx, planID)
	if err != nil {
		return err
	}
	bookings, err := q.BookingsOfPlan(ctx, planID)
	if err != nil {
		return err
	}
	b, err := reserve(mapRows(claims, toClaim), mapRows(bookings, toBooking))
	if err != nil {
		return err
	}
	return q.SetBooking(ctx, sqlcdb.SetBookingParams{
		ProviderID: b.ProviderID, PlanID: b.PlanID, Category: string(b.Category), ReservedCents: int64(b.Reserved),
	})
}
