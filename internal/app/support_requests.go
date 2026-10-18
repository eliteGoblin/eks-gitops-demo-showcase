package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/eliteGoblin/eks-gitops-demo/internal/domain"
)

// RaiseSupportRequestInput is the input of Raise.
type RaiseSupportRequestInput struct {
	PlanID              string
	Category            domain.Category
	Description         string
	EstimatedAmount     domain.Money
	PreferredProviderID string // optional
}

// DecideSupportRequestInput is the input of Decide. ProviderID overrides the
// requester's preferred provider when approving.
type DecideSupportRequestInput struct {
	ID         string
	Approve    bool
	Reason     string
	ProviderID string
}

// SupportRequestService raises, decides, lists and streams support requests.
type SupportRequestService struct {
	requests   SupportRequestRepository
	plans      PlanRepository
	providers  ProviderRepository
	planAccess planAccess
	guard      guard
	now        Clock
	nextID     IDGenerator
	events     Events
}

func requestResource(r domain.SupportRequest) Resource {
	return Resource{ParticipantID: r.ParticipantID, PlanID: r.PlanID, ProviderID: r.BookedProviderID, Category: r.Category,
		RequestedBy: r.RequestedBy, SupportRequestID: r.ID}
}

// Raise records a PENDING request on a plan the caller may act for.
func (s *SupportRequestService) Raise(ctx context.Context, in RaiseSupportRequestInput) (domain.SupportRequest, error) {
	plan, err := s.plans.GetPlan(ctx, in.PlanID)
	if errors.Is(err, domain.ErrNotFound) {
		return domain.SupportRequest{}, ErrForbidden // not found and not permitted look the same
	}
	if err != nil {
		return domain.SupportRequest{}, err
	}
	pr, err := s.guard.authorize(ctx, ActionRaiseSupportRequest, Resource{ParticipantID: plan.ParticipantID, PlanID: plan.ID, Category: in.Category})
	if err != nil {
		return domain.SupportRequest{}, err
	}
	if err := s.requireProvider(ctx, in.PreferredProviderID, "preferredProviderId"); err != nil {
		return domain.SupportRequest{}, err
	}
	r, err := domain.NewSupportRequest(domain.SupportRequestDraft{
		ID: s.nextID(), Plan: plan, RequestedBy: pr.Subject, RequestedByName: pr.Username,
		Category: in.Category, Description: in.Description, EstimatedAmount: in.EstimatedAmount,
		PreferredProviderID: in.PreferredProviderID,
	}, s.now())
	if err != nil {
		return domain.SupportRequest{}, err
	}
	if err := s.requests.InsertSupportRequest(ctx, r); err != nil {
		return domain.SupportRequest{}, fmt.Errorf("insert support request: %w", err)
	}
	s.guard.recordChange(ctx, pr, ActionRaiseSupportRequest, requestResource(r), "raised", r.Description)
	s.announce(ctx, r)
	return r, nil
}

// Decide approves or rejects a PENDING request. Staff only, and never your
// own request. An approval books the provider and reserves the estimated
// amount atomically, so concurrent approvals cannot over-reserve the budget.
// The caller is authorized before the request is looked up; that decision is
// audited with the request id the caller sent, as in ClaimService.Approve.
func (s *SupportRequestService) Decide(ctx context.Context, in DecideSupportRequestInput) (domain.SupportRequest, error) {
	pr, err := s.guard.authorize(ctx, ActionDecideSupportRequest, Resource{SupportRequestID: in.ID})
	if err != nil {
		return domain.SupportRequest{}, err
	}
	r, err := s.requests.GetSupportRequest(ctx, in.ID)
	if errors.Is(err, domain.ErrNotFound) {
		return domain.SupportRequest{}, &domain.ValidationError{Field: "id", Message: "support request not found"}
	}
	if err != nil {
		return domain.SupportRequest{}, err
	}
	if !in.Approve {
		return s.reject(ctx, pr, r, in.Reason)
	}
	return s.approve(ctx, pr, r, in)
}

func (s *SupportRequestService) approve(ctx context.Context, pr Principal, r domain.SupportRequest, in DecideSupportRequestInput) (domain.SupportRequest, error) {
	if err := r.Approve(pr.Subject, in.ProviderID, in.Reason, s.now()); err != nil {
		return domain.SupportRequest{}, err
	}
	if err := s.requireProvider(ctx, r.BookedProviderID, "providerId"); err != nil {
		return domain.SupportRequest{}, err
	}
	plan, err := s.plans.GetPlan(ctx, r.PlanID)
	if err != nil {
		return domain.SupportRequest{}, err
	}
	reserve := func(claims []domain.Claim, bookings []domain.ServiceBooking) (domain.ServiceBooking, error) {
		return plan.Reserve(r.BookedProviderID, r.Category, r.EstimatedAmount, claims, bookings)
	}
	if err := s.requests.DecideSupportRequest(ctx, r, reserve); err != nil {
		return domain.SupportRequest{}, decisionError(err)
	}
	s.guard.recordChange(ctx, pr, ActionDecideSupportRequest, requestResource(r), "approved", r.DecisionReason)
	s.announce(ctx, r)
	return r, nil
}

func (s *SupportRequestService) reject(ctx context.Context, pr Principal, r domain.SupportRequest, reason string) (domain.SupportRequest, error) {
	if err := r.Reject(pr.Subject, reason, s.now()); err != nil {
		return domain.SupportRequest{}, err
	}
	if err := s.requests.DecideSupportRequest(ctx, r, nil); err != nil {
		return domain.SupportRequest{}, decisionError(err)
	}
	s.guard.recordChange(ctx, pr, ActionDecideSupportRequest, requestResource(r), "rejected", r.DecisionReason)
	s.announce(ctx, r)
	return r, nil
}

func decisionError(err error) error {
	if isBusinessRule(err) {
		return err
	}
	return fmt.Errorf("store decision: %w", err)
}

// requireProvider checks that an optional provider id names a provider.
func (s *SupportRequestService) requireProvider(ctx context.Context, id, field string) error {
	if id == "" {
		return nil
	}
	_, err := s.providers.GetProvider(ctx, id)
	if errors.Is(err, domain.ErrNotFound) {
		return &domain.ValidationError{Field: field, Message: "provider not found"}
	}
	return err
}

// Mine lists the requests the caller raised or that concern the participants
// it is or acts for, oldest first.
func (s *SupportRequestService) Mine(ctx context.Context) ([]domain.SupportRequest, error) {
	pr, err := s.guard.principal(ctx)
	if err != nil {
		return nil, err
	}
	scope, err := s.guard.policy.SupportRequestScope(ctx, pr)
	if err != nil {
		return nil, err
	}
	return s.list(ctx, pr, ActionReadSupportRequests, SupportRequestQuery{Scope: scope})
}

// Pending is the staff work queue: every PENDING request, oldest first.
func (s *SupportRequestService) Pending(ctx context.Context) ([]domain.SupportRequest, error) {
	pr, err := s.guard.authorize(ctx, ActionReadSupportQueue, Resource{})
	if err != nil {
		return nil, err
	}
	pending := domain.SupportRequestPending
	return s.list(ctx, pr, ActionReadSupportQueue, SupportRequestQuery{Scope: Scope{All: true}, Status: &pending})
}

func (s *SupportRequestService) list(ctx context.Context, pr Principal, a Action, q SupportRequestQuery) ([]domain.SupportRequest, error) {
	out, err := s.requests.ListSupportRequests(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list support requests: %w", err)
	}
	s.guard.recordList(ctx, pr, a, Resource{}, q.Scope, len(out), len(out), "support requests")
	return out, nil
}

// Get returns the support request, or nil when it does not exist or the
// caller may not read it (the rules of mySupportRequests, plus staff).
func (s *SupportRequestService) Get(ctx context.Context, id string) (*domain.SupportRequest, error) {
	r, err := s.requests.GetSupportRequest(ctx, id)
	if err != nil {
		return nil, hideDenied(err)
	}
	if _, err := s.guard.authorize(ctx, ActionReadSupportRequest, requestResource(r)); err != nil {
		return nil, hideDenied(err)
	}
	return &r, nil
}

// visible returns, aligned to found, each support request the caller may
// read, or nil: the rule of Get.
func (s *SupportRequestService) visible(ctx context.Context, found []domain.SupportRequest) ([]*domain.SupportRequest, error) {
	if _, err := s.guard.principal(ctx); err != nil {
		return nil, err
	}
	return visibleOf(ctx, s.guard, found, func(r domain.SupportRequest) (Action, Resource) {
		return ActionReadSupportRequest, requestResource(r)
	})
}

// Watch streams the support requests on planID as they are raised and
// decided. The caller must be able to read the plan; every change is re-read
// with the caller's access. The stream ends when ctx does.
func (s *SupportRequestService) Watch(ctx context.Context, planID string) (<-chan domain.SupportRequest, error) {
	if err := s.planAccess.requireRead(ctx, planID); err != nil {
		return nil, err
	}
	events, err := s.events.Subscribe(ctx, SupportRequestChanged, planID)
	if err != nil {
		return nil, err
	}
	return watch(ctx, events, s.Get), nil
}

func (s *SupportRequestService) announce(ctx context.Context, r domain.SupportRequest) {
	s.events.Publish(ctx, Event{Kind: SupportRequestChanged, ID: r.ID, PlanID: r.PlanID})
}
