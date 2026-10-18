package domain

import (
	"errors"
	"testing"
)

func approvedClaim(t *testing.T) Claim {
	t.Helper()
	c := Claim{ID: "C1", PlanID: "PL1", ProviderID: "PR1", Amount: 120_50, Status: ClaimSubmitted}
	if err := c.Approve(); err != nil {
		t.Fatal(err)
	}
	return c
}

// An approved claim's payment goes PENDING → PAID, and the claim becomes PAID.
func TestPaymentLifecycleToPaid(t *testing.T) {
	c := approvedClaim(t)
	if c.Payment != nil {
		t.Fatalf("an approval alone requests no payment, got %+v", c.Payment)
	}
	if err := c.StartPayment(); err != nil || c.Payment.Status != PaymentPending || c.Status != ClaimApproved {
		t.Fatalf("start: %v %+v status %s", err, c.Payment, c.Status)
	}
	if err := c.MarkPaid("pay_1"); err != nil {
		t.Fatal(err)
	}
	if c.Status != ClaimPaid || c.Payment.Status != PaymentPaid || c.Payment.Ref != "pay_1" {
		t.Fatalf("paid claim = status %s payment %+v", c.Status, c.Payment)
	}
}

// A failed payment leaves the approval standing.
func TestPaymentLifecycleToFailed(t *testing.T) {
	c := approvedClaim(t)
	if err := c.StartPayment(); err != nil {
		t.Fatal(err)
	}
	if err := c.MarkPaymentFailed(); err != nil {
		t.Fatal(err)
	}
	if c.Status != ClaimApproved || c.Payment.Status != PaymentFailed || c.Payment.Ref != "" {
		t.Fatalf("failed payment = status %s payment %+v", c.Status, c.Payment)
	}
}

// Payment steps out of order are refused: only an approved claim without a
// payment may start one, and only a pending payment may settle.
func TestPaymentStepsOutOfOrderAreRefused(t *testing.T) {
	submitted := Claim{ID: "C2", Status: ClaimSubmitted}
	for name, step := range map[string]func(c *Claim) error{
		"start on a submitted claim": func(c *Claim) error { return c.StartPayment() },
		"pay without a pending payment": func(c *Claim) error {
			x := approvedClaim(t)
			*c = x
			return c.MarkPaid("pay_1")
		},
		"fail without a pending payment": func(c *Claim) error {
			x := approvedClaim(t)
			*c = x
			return c.MarkPaymentFailed()
		},
		"start twice": func(c *Claim) error {
			x := approvedClaim(t)
			*c = x
			_ = c.StartPayment()
			return c.StartPayment()
		},
	} {
		c := submitted
		if err := step(&c); !errors.Is(err, ErrPaymentState) {
			t.Errorf("%s: want ErrPaymentState, got %v", name, err)
		}
	}
}
