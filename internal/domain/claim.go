package domain

import (
	"errors"
	"fmt"
	"time"
)

// Claim is a provider's request to be paid from a plan budget.
type Claim struct {
	ID          string
	PlanID      string
	ProviderID  string
	Category    Category
	Amount      Money
	Status      ClaimStatus
	ServiceDate Date
	SubmittedAt time.Time     // UTC
	Payment     *ClaimPayment // nil until a payment is requested
}

// PaymentStatus is where an approved claim's payment stands.
type PaymentStatus string

const (
	PaymentPending PaymentStatus = "PENDING" // requested; the outcome is not known yet
	PaymentPaid    PaymentStatus = "PAID"    // the payment processor accepted it
	PaymentFailed  PaymentStatus = "FAILED"  // refused, or the processor could not be reached
)

// ClaimPayment is the payment of an approved claim.
type ClaimPayment struct {
	Status PaymentStatus
	Ref    string // the processor's payment id, once it accepted
}

// ErrPaymentState marks a payment step taken out of order, such as paying a
// claim that was never approved.
var ErrPaymentState = errors.New("claim is not in a state for this payment step")

// NewClaim builds a SUBMITTED claim after validating the amount.
func NewClaim(id, planID, providerID string, c Category, amount Money, serviceDate Date, now time.Time) (Claim, error) {
	if amount <= 0 {
		return Claim{}, &ValidationError{Field: "amount", Message: "amount must be greater than zero"}
	}
	return Claim{
		ID:          id,
		PlanID:      planID,
		ProviderID:  providerID,
		Category:    c,
		Amount:      amount,
		Status:      ClaimSubmitted,
		ServiceDate: serviceDate,
		SubmittedAt: now.UTC(),
	}, nil
}

// Approve moves a SUBMITTED claim to APPROVED; any other state is rejected.
func (c *Claim) Approve() error {
	if c.Status != ClaimSubmitted {
		return ClaimNotInStatus(ClaimSubmitted)
	}
	c.Status = ClaimApproved
	return nil
}

// StartPayment records that the approved claim's payment was requested.
func (c *Claim) StartPayment() error {
	if c.Status != ClaimApproved || c.Payment != nil {
		return fmt.Errorf("%w: start a payment for claim %s (status %s)", ErrPaymentState, c.ID, c.Status)
	}
	c.Payment = &ClaimPayment{Status: PaymentPending}
	return nil
}

// MarkPaid records the processor's acceptance under ref: the claim is PAID.
func (c *Claim) MarkPaid(ref string) error {
	if err := c.requirePendingPayment("pay"); err != nil {
		return err
	}
	c.Status = ClaimPaid
	c.Payment = &ClaimPayment{Status: PaymentPaid, Ref: ref}
	return nil
}

// MarkPaymentFailed records that the payment did not go through. The
// approval stands: the claim stays APPROVED.
func (c *Claim) MarkPaymentFailed() error {
	if err := c.requirePendingPayment("fail the payment of"); err != nil {
		return err
	}
	c.Payment = &ClaimPayment{Status: PaymentFailed}
	return nil
}

func (c *Claim) requirePendingPayment(step string) error {
	if c.Status != ClaimApproved || c.Payment == nil || c.Payment.Status != PaymentPending {
		return fmt.Errorf("%w: %s claim %s without a pending payment", ErrPaymentState, step, c.ID)
	}
	return nil
}

// ClaimNotInStatus is the rule failure for a transition from a status the
// claim is no longer in (e.g. a second approval); stores return it too.
func ClaimNotInStatus(want ClaimStatus) error {
	return &ValidationError{Field: "id", Message: fmt.Sprintf("claim is not in %s state", want)}
}
