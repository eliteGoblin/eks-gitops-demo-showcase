package app

import (
	"context"
	"errors"

	"github.com/eliteGoblin/eks-gitops-demo/internal/domain"
)

// PlanService reads plans and derives their budgets.
type PlanService struct {
	plans    PlanRepository
	claims   ClaimRepository
	bookings BookingRepository
	guard    guard
}

// Get returns the plan, or nil when it does not exist or is not permitted.
func (s *PlanService) Get(ctx context.Context, id string) (*domain.Plan, error) {
	plan, err := s.plans.GetPlan(ctx, id)
	if err != nil {
		return nil, hideDenied(err)
	}
	if _, err := s.guard.authorize(ctx, ActionReadPlan, Resource{ParticipantID: plan.ParticipantID, PlanID: plan.ID}); err != nil {
		return nil, hideDenied(err)
	}
	return &plan, nil
}

// GetMany returns the plans for ids in the same order, nil where a plan does
// not exist or may not be read. It reads the repository once (DataLoaders).
func (s *PlanService) GetMany(ctx context.Context, ids []string) ([]*domain.Plan, error) {
	if _, err := s.guard.principal(ctx); err != nil {
		return nil, err
	}
	found, err := s.plans.GetPlans(ctx, ids)
	if err != nil {
		return nil, err
	}
	shown, err := s.visible(ctx, found)
	if err != nil {
		return nil, err
	}
	return alignTo(ids, found, shown, planID), nil
}

// visible returns, aligned to found, each plan the caller may read, or nil.
func (s *PlanService) visible(ctx context.Context, found []domain.Plan) ([]*domain.Plan, error) {
	if _, err := s.guard.principal(ctx); err != nil {
		return nil, err
	}
	return visibleOf(ctx, s.guard, found, func(p domain.Plan) (Action, Resource) {
		return ActionReadPlan, Resource{ParticipantID: p.ParticipantID, PlanID: p.ID}
	})
}

func planID(p domain.Plan) string { return p.ID }

// ListByParticipant returns the participant's plans the caller may read.
func (s *PlanService) ListByParticipant(ctx context.Context, participantID string) ([]domain.Plan, error) {
	all, err := s.plans.ListPlansByParticipant(ctx, participantID)
	if err != nil {
		return nil, err
	}
	visible := make([]domain.Plan, 0, len(all))
	for _, plan := range all {
		_, err := s.guard.authorize(ctx, ActionReadPlan, Resource{ParticipantID: plan.ParticipantID, PlanID: plan.ID})
		if errors.Is(err, ErrForbidden) {
			continue
		}
		if err != nil {
			return nil, err
		}
		visible = append(visible, plan)
	}
	return visible, nil
}

// Budgets returns the plan's budgets, keeping only the categories the caller
// may see (providers: booked categories only).
func (s *PlanService) Budgets(ctx context.Context, plan domain.Plan) ([]domain.Budget, error) {
	pr, err := s.guard.authorize(ctx, ActionReadPlan, Resource{ParticipantID: plan.ParticipantID, PlanID: plan.ID})
	if err != nil {
		return nil, err
	}
	claims, err := s.claims.ClaimsOfPlan(ctx, plan.ID)
	if err != nil {
		return nil, err
	}
	bookings, err := s.bookings.BookingsOfPlan(ctx, plan.ID)
	if err != nil {
		return nil, err
	}
	out := []domain.Budget{}
	for _, b := range plan.Budgets(claims, bookings) {
		ok, err := s.guard.policy.CanSeeBudget(ctx, pr, Resource{ParticipantID: plan.ParticipantID, PlanID: plan.ID, Category: b.Category})
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, b)
		}
	}
	return out, nil
}
