package app

import (
	"context"
	"errors"

	"github.com/eliteGoblin/eks-gitops-demo/internal/domain"
)

// planAccess checks that the caller may read a plan before it may follow it.
type planAccess struct {
	plans PlanRepository
	guard guard
}

// requireRead returns ErrForbidden when the plan does not exist or the caller
// may not read it (the two look the same), ErrUnauthenticated without a caller.
func (a planAccess) requireRead(ctx context.Context, planID string) error {
	if _, err := a.guard.principal(ctx); err != nil {
		return err
	}
	plan, err := a.plans.GetPlan(ctx, planID)
	if errors.Is(err, domain.ErrNotFound) {
		return ErrForbidden
	}
	if err != nil {
		return err
	}
	_, err = a.guard.authorize(ctx, ActionReadPlan, Resource{ParticipantID: plan.ParticipantID, PlanID: plan.ID})
	return err
}
