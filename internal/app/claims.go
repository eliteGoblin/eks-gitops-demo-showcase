package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/eliteGoblin/eks-gitops-demo/internal/domain"
)

// ClaimFilter narrows a claim listing; the fields that are set combine with AND.
type ClaimFilter struct {
	PlanID          string // empty = all plans
	Status          *domain.ClaimStatus
	Category        *domain.Category
	ProviderID      string     // empty = any provider
	SubmittedAfter  *time.Time // inclusive
	SubmittedBefore *time.Time // exclusive
}

// SubmitClaimInput is the input of SubmitClaim.
type SubmitClaimInput struct {
	PlanID      string
	Category    domain.Category
	Amount      domain.Money
	ServiceDate domain.Date
	// IdempotencyKey makes a retry return the claim the first request
	// created; empty means the request is not retry-safe.
	IdempotencyKey string
}

// Page asks for First claims in Order, after an optional keyset cursor that
// must have been issued for the same order.
type Page struct {
	First int
	After *ClaimCursor
	Order ClaimOrder
}

// NewClaimCursor is the keyset position of c in order o.
func NewClaimCursor(c domain.Claim, o ClaimOrder) ClaimCursor {
	return ClaimCursor{Order: o.Normalized(), SubmittedAt: c.SubmittedAt, Amount: c.Amount, ID: c.ID}
}

// ClaimPage is one page of a listing plus what the client needs to continue.
type ClaimPage struct {
	Claims  []domain.Claim
	HasNext bool
	Total   int
}

// isBusinessRule reports whether err is a domain rule failure the caller can act on.
func isBusinessRule(err error) bool {
	var ve *domain.ValidationError
	var be *domain.BudgetExceededError
	var se *domain.SelfApprovalNotAllowedError
	return errors.As(err, &ve) || errors.As(err, &be) || errors.As(err, &se)
}

// ClaimService lists, submits, approves and pays claims.
type ClaimService struct {
	claims     ClaimRepository
	plans      PlanRepository
	planAccess planAccess
	guard      guard
	now        Clock
	nextID     IDGenerator
	payments   PaymentGateway // nil: approved claims are not paid
	events     Events
}

// schemeCurrency is the only currency the scheme pays in.
const schemeCurrency = "AUD"

func claimResource(c domain.Claim, plan domain.Plan) Resource {
	return Resource{ParticipantID: plan.ParticipantID, PlanID: plan.ID, ProviderID: c.ProviderID, Category: c.Category, ClaimID: c.ID}
}

// paymentWriteRetryTimeout bounds the one retry of storing a payment result.
const paymentWriteRetryTimeout = 2 * time.Second

// List returns one page of the claims the caller may read, in the page's
// order. The policy turns the caller into a scope that the repository
// applies, so nothing outside it is loaded; the listing is audited once.
func (s *ClaimService) List(ctx context.Context, f ClaimFilter, p Page) (ClaimPage, error) {
	if p.First < 1 {
		return ClaimPage{}, fmt.Errorf("%w: first must be at least 1", domain.ErrInvalidInput)
	}
	pr, err := s.guard.principal(ctx)
	if err != nil {
		return ClaimPage{}, err
	}
	scope, err := s.guard.policy.ClaimScope(ctx, pr)
	if err != nil {
		return ClaimPage{}, err
	}
	order := p.Order.Normalized()
	if p.After != nil {
		if err := s.checkCursor(ctx, scope, order, *p.After); err != nil {
			return ClaimPage{}, err
		}
	}
	q := ClaimQuery{Scope: scope, Filter: f, Order: order, After: p.After, Limit: p.First + 1}
	page, err := s.query(ctx, q)
	if err != nil {
		return ClaimPage{}, err
	}
	s.guard.recordList(ctx, pr, ActionReadClaim, Resource{PlanID: f.PlanID}, scope, len(page.Claims), page.Total, "claims")
	return page, nil
}

func (s *ClaimService) query(ctx context.Context, q ClaimQuery) (ClaimPage, error) {
	if q.Scope.Empty() {
		return ClaimPage{Claims: []domain.Claim{}}, nil
	}
	claims, err := s.claims.QueryClaims(ctx, q)
	if err != nil {
		return ClaimPage{}, fmt.Errorf("query claims: %w", err)
	}
	total, err := s.claims.CountClaims(ctx, q)
	if err != nil {
		return ClaimPage{}, fmt.Errorf("count claims: %w", err)
	}
	hasNext := len(claims) == q.Limit
	if hasNext {
		claims = claims[:q.Limit-1]
	}
	return ClaimPage{Claims: claims, HasNext: hasNext, Total: total}, nil
}

// checkCursor rejects a cursor issued for another order, or one that does not
// point at a claim the caller can see: following it would silently skip or
// repeat records.
func (s *ClaimService) checkCursor(ctx context.Context, scope Scope, order ClaimOrder, c ClaimCursor) error {
	if c.Order.Normalized() != order {
		return fmt.Errorf("%w: cursor was issued for a different order", domain.ErrInvalidInput)
	}
	stale := fmt.Errorf("%w: cursor does not match a visible claim", domain.ErrInvalidInput)
	claim, err := s.claims.GetClaim(ctx, c.ID)
	if errors.Is(err, domain.ErrNotFound) {
		return stale
	}
	if err != nil {
		return err
	}
	if !cursorKeyMatches(claim, c) {
		return stale
	}
	plan, err := s.plans.GetPlan(ctx, claim.PlanID)
	if err != nil {
		return err
	}
	if !scope.matchesClaim(claim, plan.ParticipantID) {
		return stale
	}
	return nil
}

// cursorKeyMatches reports whether the cursor's sort key is still the claim's.
func cursorKeyMatches(claim domain.Claim, c ClaimCursor) bool {
	if c.Order.Normalized().Field == OrderByAmount {
		return claim.Amount == c.Amount
	}
	return claim.SubmittedAt.Equal(c.SubmittedAt)
}

// Submit creates a SUBMITTED claim for the caller's provider. Business rule
// failures come back as *domain.ValidationError, *domain.BudgetExceededError
// or *IdempotencyKeyReusedError. With an idempotency key, a retry of the same
// request returns the claim the first one created.
func (s *ClaimService) Submit(ctx context.Context, in SubmitClaimInput) (domain.Claim, error) {
	if in.Amount <= 0 {
		return domain.Claim{}, &domain.ValidationError{Field: "amount", Message: "amount must be greater than zero"}
	}
	if err := validateIdempotencyKey(in.IdempotencyKey); err != nil {
		return domain.Claim{}, err
	}
	plan, err := s.plans.GetPlan(ctx, in.PlanID)
	if errors.Is(err, domain.ErrNotFound) {
		return domain.Claim{}, ErrForbidden // not found and not permitted look the same
	}
	if err != nil {
		return domain.Claim{}, err
	}
	pr, err := s.guard.authorize(ctx, ActionSubmitClaim, Resource{ParticipantID: plan.ParticipantID, PlanID: plan.ID, Category: in.Category})
	if err != nil {
		return domain.Claim{}, err
	}
	c, err := domain.NewClaim(s.nextID(), plan.ID, pr.ProviderID, in.Category, in.Amount, in.ServiceDate, s.now())
	if err != nil {
		return domain.Claim{}, err
	}
	withinBudget := func(planClaims []domain.Claim, bookings []domain.ServiceBooking) error {
		return plan.CheckClaim(pr.ProviderID, in.Category, in.Amount, in.ServiceDate, planClaims, bookings)
	}
	if in.IdempotencyKey != "" {
		key := IdempotencyKey{Subject: pr.Subject, Key: in.IdempotencyKey, Fingerprint: in.fingerprint()}
		prior, replayed, err := s.claims.InsertClaimOnce(ctx, c, key, withinBudget)
		if err != nil {
			return domain.Claim{}, insertFailure(err)
		}
		if replayed {
			return s.replay(ctx, pr, plan, key, prior)
		}
	} else if err := s.claims.InsertClaimWithinBudget(ctx, c, withinBudget); err != nil {
		return domain.Claim{}, insertFailure(err)
	}
	s.guard.recordChange(ctx, pr, ActionSubmitClaim, claimResource(c, plan), "submitted", "")
	s.announce(ctx, c)
	return c, nil
}

// insertFailure passes a business rule failure through and wraps anything else.
func insertFailure(err error) error {
	if isBusinessRule(err) {
		return err
	}
	return fmt.Errorf("insert claim: %w", err)
}

// replay answers a retry: the claim the key first created when the request
// is the same, an *IdempotencyKeyReusedError when it is not. Nothing is
// stored, announced or paid again.
func (s *ClaimService) replay(ctx context.Context, pr Principal, plan domain.Plan, key IdempotencyKey, prior IdempotencyRecord) (domain.Claim, error) {
	if prior.Fingerprint != key.Fingerprint {
		return domain.Claim{}, &IdempotencyKeyReusedError{}
	}
	c, err := s.claims.GetClaim(ctx, prior.ClaimID)
	if err != nil {
		return domain.Claim{}, fmt.Errorf("read the claim of a replayed request: %w", err)
	}
	s.guard.recordChange(ctx, pr, ActionSubmitClaim, claimResource(c, plan), "replayed", "")
	return c, nil
}

// Approve moves a SUBMITTED claim to APPROVED. The reason is required and audited.
// The caller is authorized before the claim is looked up, so a caller who may
// not approve cannot learn which claim ids exist. That decision is audited
// with the claim id the caller sent (the policy never decides on an id); its
// plan and parties stay empty because nothing has been read yet, and the
// outcome events that follow name them.
func (s *ClaimService) Approve(ctx context.Context, id, reason string) (domain.Claim, error) {
	if strings.TrimSpace(reason) == "" {
		return domain.Claim{}, &domain.ValidationError{Field: "reason", Message: "reason is required"}
	}
	pr, err := s.guard.authorize(ctx, ActionApproveClaim, Resource{ClaimID: id})
	if err != nil {
		return domain.Claim{}, err
	}
	c, err := s.claims.GetClaim(ctx, id)
	if errors.Is(err, domain.ErrNotFound) {
		return domain.Claim{}, &domain.ValidationError{Field: "id", Message: "claim not found"}
	}
	if err != nil {
		return domain.Claim{}, err
	}
	plan, err := s.plans.GetPlan(ctx, c.PlanID)
	if err != nil {
		return domain.Claim{}, err
	}
	if err := c.Approve(); err != nil {
		return domain.Claim{}, err
	}
	if s.payments != nil {
		if err := c.StartPayment(); err != nil {
			return domain.Claim{}, err
		}
	}
	// Conditional on the stored status: a concurrent approval loses here, so
	// only the winner goes on to pay.
	if err := s.claims.UpdateClaim(ctx, c, domain.ClaimSubmitted); err != nil {
		if isBusinessRule(err) {
			return domain.Claim{}, err
		}
		return domain.Claim{}, fmt.Errorf("update claim: %w", err)
	}
	s.guard.recordChange(ctx, pr, ActionApproveClaim, claimResource(c, plan), "approved", reason)
	s.announce(ctx, c)
	if s.payments == nil {
		return c, nil
	}
	return s.pay(ctx, pr, c, plan)
}

// pay asks the processor to pay an approved claim, with the claim id as the
// idempotency key. The approval stands whatever happens: a refusal or an
// unreachable processor is recorded as a FAILED payment (paying it again
// later is out of scope), never rolled back. It runs detached from the
// caller's cancellation, so a client that disconnects mid-payment cannot
// strand the claim in PENDING; the gateway bounds how long it may take.
func (s *ClaimService) pay(ctx context.Context, pr Principal, c domain.Claim, plan domain.Plan) (domain.Claim, error) {
	ctx = context.WithoutCancel(ctx)
	receipt, payErr := s.payments.RequestPayment(ctx, PaymentRequest{
		IdempotencyKey: c.ID, Reference: c.ID, Payee: c.ProviderID, AmountCents: int64(c.Amount), Currency: schemeCurrency,
	})
	verb, detail := "paid", receipt.PaymentID
	if payErr != nil {
		verb, detail = "payment_failed", paymentFailure(payErr)
		if err := c.MarkPaymentFailed(); err != nil {
			return domain.Claim{}, err
		}
	} else if err := c.MarkPaid(receipt.PaymentID); err != nil {
		return domain.Claim{}, err
	}
	// The processor's answer goes on record before it is stored, so the
	// payment reference survives a failed write.
	s.guard.recordChange(ctx, pr, ActionApproveClaim, claimResource(c, plan), verb, detail)
	if err := s.storePaymentResult(ctx, c); err != nil {
		// The store's error is kept as text, not wrapped: whatever it said (even
		// a business rule, when the claim already holds another payment) this
		// is a server failure for operators, not a validation error that reads
		// as "try again".
		return domain.Claim{}, fmt.Errorf("%w: payment %q (%s) of claim %s: %s", ErrPaymentNotRecorded, receipt.PaymentID, verb, c.ID, err.Error())
	}
	s.announce(ctx, c)
	return c, nil
}

// storePaymentResult writes the paid or failed claim, retrying once after a
// storage failure: the money may already have moved, so one blip must not
// lose the record of it. A business rule failure is not retried.
func (s *ClaimService) storePaymentResult(ctx context.Context, c domain.Claim) error {
	err := s.claims.UpdateClaim(ctx, c, domain.ClaimApproved)
	if err == nil || isBusinessRule(err) {
		return err
	}
	retry, cancel := context.WithTimeout(context.WithoutCancel(ctx), paymentWriteRetryTimeout)
	defer cancel()
	err = s.claims.UpdateClaim(retry, c, domain.ClaimApproved)
	if isBusinessRule(err) && s.alreadyStored(retry, c) {
		return nil // the first write was stored after all; only its acknowledgement was lost
	}
	return err
}

// alreadyStored reports whether the stored claim already has c's status and
// payment.
func (s *ClaimService) alreadyStored(ctx context.Context, c domain.Claim) bool {
	stored, err := s.claims.GetClaim(ctx, c.ID)
	return err == nil && stored.Status == c.Status && samePayment(stored.Payment, c.Payment)
}

func samePayment(a, b *domain.ClaimPayment) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// paymentFailure classifies a payment error for the audit trail without
// copying transport details (addresses, URLs) into it.
func paymentFailure(err error) string {
	switch {
	case errors.Is(err, ErrPaymentRejected):
		return "rejected"
	case errors.Is(err, ErrPaymentUnavailable):
		return "unavailable"
	default:
		return "error"
	}
}

// Get returns the claim, or nil when it does not exist or may not be read.
func (s *ClaimService) Get(ctx context.Context, id string) (*domain.Claim, error) {
	c, err := s.claims.GetClaim(ctx, id)
	if err != nil {
		return nil, hideDenied(err)
	}
	plan, err := s.plans.GetPlan(ctx, c.PlanID)
	if err != nil {
		return nil, hideDenied(err)
	}
	if _, err := s.guard.authorize(ctx, ActionReadClaim, claimResource(c, plan)); err != nil {
		return nil, hideDenied(err)
	}
	return &c, nil
}

// visible returns, aligned to found, each claim the caller may read, or nil:
// the rule of Get, with the claims' plans read in one batch.
func (s *ClaimService) visible(ctx context.Context, found []domain.Claim) ([]*domain.Claim, error) {
	if _, err := s.guard.principal(ctx); err != nil {
		return nil, err
	}
	planIDs := make([]string, 0, len(found))
	for _, c := range found {
		if !slices.Contains(planIDs, c.PlanID) {
			planIDs = append(planIDs, c.PlanID)
		}
	}
	plans, err := s.plans.GetPlans(ctx, planIDs)
	if err != nil {
		return nil, err
	}
	planOf := indexBy(plans, planID)
	out := make([]*domain.Claim, len(found))
	for i := range found {
		plan, ok := planOf[found[i].PlanID]
		if !ok {
			continue // hidden, as Get hides a claim whose plan is missing
		}
		_, err := s.guard.authorize(ctx, ActionReadClaim, claimResource(found[i], plan))
		if errors.Is(err, ErrForbidden) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out[i] = &found[i]
	}
	return out, nil
}

// Watch streams the claims on planID as they are submitted, approved and
// paid. The caller must be able to read the plan; every change is re-read
// with the caller's access, so claims it may not see are skipped. The stream
// ends when ctx does.
func (s *ClaimService) Watch(ctx context.Context, planID string) (<-chan domain.Claim, error) {
	if err := s.planAccess.requireRead(ctx, planID); err != nil {
		return nil, err
	}
	events, err := s.events.Subscribe(ctx, ClaimChanged, planID)
	if err != nil {
		return nil, err
	}
	return watch(ctx, events, s.Get), nil
}

func (s *ClaimService) announce(ctx context.Context, c domain.Claim) {
	s.events.Publish(ctx, Event{Kind: ClaimChanged, ID: c.ID, PlanID: c.PlanID})
}
