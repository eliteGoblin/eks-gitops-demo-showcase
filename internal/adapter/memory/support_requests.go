package memory

import (
	"context"
	"slices"
	"sort"

	"github.com/eliteGoblin/eks-gitops-demo/internal/app"
	"github.com/eliteGoblin/eks-gitops-demo/internal/domain"
)

func (s *Store) InsertSupportRequest(_ context.Context, r domain.SupportRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests[r.ID] = r
	return nil
}

func (s *Store) GetSupportRequest(_ context.Context, id string) (domain.SupportRequest, error) {
	return get(&s.mu, s.requests, id)
}

func (s *Store) GetSupportRequests(_ context.Context, ids []string) ([]domain.SupportRequest, error) {
	return getMany(&s.mu, s.requests, ids, func(r domain.SupportRequest) domain.SupportRequest { return r }), nil
}

// ListSupportRequests returns the requests in q's scope, oldest first.
func (s *Store) ListSupportRequests(_ context.Context, q app.SupportRequestQuery) ([]domain.SupportRequest, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []domain.SupportRequest{}
	for _, r := range s.requests {
		if q.Status != nil && r.Status != *q.Status {
			continue
		}
		if q.Scope.All || r.RequestedBy == q.Scope.RequestedBy || slices.Contains(q.Scope.ParticipantIDs, r.ParticipantID) {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].RaisedAt.Equal(out[j].RaisedAt) {
			return out[i].RaisedAt.Before(out[j].RaisedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// DecideSupportRequest stores the decision and, for an approval, the
// reservation, under one write lock: the PENDING check, the budget check and
// both writes cannot interleave with another decision.
func (s *Store) DecideSupportRequest(_ context.Context, r domain.SupportRequest, reserve app.Reservation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, ok := s.requests[r.ID]
	if !ok {
		return domain.ErrNotFound
	}
	if stored.Status != domain.SupportRequestPending {
		return &domain.ValidationError{Field: "id", Message: "support request is already decided"}
	}
	if r.Status == domain.SupportRequestApproved {
		booking, err := reserve(s.claimsOfPlan(r.PlanID), s.bookingsOfPlan(r.PlanID))
		if err != nil {
			return err
		}
		s.setBooking(booking)
	}
	s.requests[r.ID] = r
	return nil
}

// setBooking stores b, replacing the provider's booking on that plan and
// category if there is one. The caller holds s.mu.
func (s *Store) setBooking(b domain.ServiceBooking) {
	i := slices.IndexFunc(s.bookings, func(x domain.ServiceBooking) bool {
		return x.ProviderID == b.ProviderID && x.PlanID == b.PlanID && x.Category == b.Category
	})
	if i < 0 {
		s.bookings = append(s.bookings, b)
		return
	}
	s.bookings[i] = b
}
