package app_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/eliteGoblin/eks-gitops-demo/internal/app"
	"github.com/eliteGoblin/eks-gitops-demo/internal/domain"
)

// Approving a claim pays it: the processor gets the claim id as the
// idempotency key, and the claim becomes PAID with the processor's reference.
func TestApprovalPaysTheClaim(t *testing.T) {
	forEachStore(t, func(t *testing.T, env0 testEnv) {
		gw := &fakeGateway{}
		ev := newFakeEvents()
		env := newEnvWith(t, env0.store, gw, ev)
		staff := as(app.RoleAgencyStaff, "")

		c, err := env.svc.Claims.Approve(staff, "C2", "verified invoice")
		if err != nil {
			t.Fatal(err)
		}
		if c.Status != domain.ClaimPaid || c.Payment == nil || c.Payment.Status != domain.PaymentPaid || c.Payment.Ref != "pay_C2" {
			t.Fatalf("approved claim = status %s payment %+v", c.Status, c.Payment)
		}
		reqs := gw.requests()
		if len(reqs) != 1 || reqs[0] != (app.PaymentRequest{IdempotencyKey: "C2", Reference: "C2", Payee: "PR1", AmountCents: 180_50, Currency: "AUD"}) {
			t.Fatalf("payment requests = %+v", reqs)
		}
		stored, err := env.store.GetClaim(context.Background(), "C2")
		if err != nil || stored.Status != domain.ClaimPaid || stored.Payment == nil || stored.Payment.Ref != "pay_C2" {
			t.Fatalf("stored claim = %+v %v", stored, err)
		}
		if !env.audit.has(app.ActionApproveClaim, "approved", "verified invoice") || !env.audit.has(app.ActionApproveClaim, "paid", "pay_C2") {
			t.Fatalf("approval and payment must both be audited, got %+v", env.audit.events)
		}
		if got := ev.kinds(); len(got) != 2 || got[0] != (app.Event{Kind: app.ClaimChanged, ID: "C2", PlanID: "PL1"}) || got[1] != got[0] {
			t.Fatalf("approval and payment must each announce the claim, got %+v", got)
		}
	})
}

// A refused or unreachable payment never undoes the approval: the claim stays
// APPROVED with a FAILED payment, and the outcome is audited.
func TestFailedPaymentLeavesTheApprovalStanding(t *testing.T) {
	for name, payErr := range map[string]error{
		"processor unavailable (or breaker open)": app.ErrPaymentUnavailable,
		"processor refused":                       app.ErrPaymentRejected,
	} {
		t.Run(name, func(t *testing.T) {
			forEachStore(t, func(t *testing.T, env0 testEnv) {
				gw := &fakeGateway{err: payErr}
				env := newEnvWith(t, env0.store, gw, nil)
				c, err := env.svc.Claims.Approve(as(app.RoleAgencyStaff, ""), "C2", "verified invoice")
				if err != nil {
					t.Fatalf("a failed payment must not fail the approval: %v", err)
				}
				if c.Status != domain.ClaimApproved || c.Payment == nil || c.Payment.Status != domain.PaymentFailed {
					t.Fatalf("claim = status %s payment %+v", c.Status, c.Payment)
				}
				stored, _ := env.store.GetClaim(context.Background(), "C2")
				if stored.Status != domain.ClaimApproved || stored.Payment == nil || stored.Payment.Status != domain.PaymentFailed {
					t.Fatalf("stored claim = %+v", stored)
				}
				if !env.audit.has(app.ActionApproveClaim, "payment_failed", outcomeDetail(payErr)) {
					t.Fatalf("the failed payment must be audited, got %+v", env.audit.events)
				}
			})
		})
	}
}

func outcomeDetail(err error) string {
	if errors.Is(err, app.ErrPaymentRejected) {
		return "rejected"
	}
	return "unavailable"
}

// Approving again is refused before any payment: one claim, one payment.
func TestReplayedApprovalDoesNotPayTwice(t *testing.T) {
	forEachStore(t, func(t *testing.T, env0 testEnv) {
		gw := &fakeGateway{}
		env := newEnvWith(t, env0.store, gw, nil)
		staff := as(app.RoleAgencyStaff, "")
		if _, err := env.svc.Claims.Approve(staff, "C2", "verified invoice"); err != nil {
			t.Fatal(err)
		}
		var ve *domain.ValidationError
		if _, err := env.svc.Claims.Approve(staff, "C2", "again"); !errors.As(err, &ve) {
			t.Fatalf("a second approval must be a ValidationError, got %v", err)
		}
		if n := len(gw.requests()); n != 1 {
			t.Fatalf("payment requests = %d, want 1", n)
		}
	})
}

// Concurrent approvals of one claim: exactly one wins, and only it pays.
func TestConcurrentApprovalsPayOnce(t *testing.T) {
	forEachStore(t, func(t *testing.T, env0 testEnv) {
		gw := &fakeGateway{}
		env := newEnvWith(t, env0.store, gw, nil)
		staff := as(app.RoleAgencyStaff, "")
		var wg sync.WaitGroup
		var mu sync.Mutex
		won := 0
		for range 20 {
			wg.Go(func() {
				if _, err := env.svc.Claims.Approve(staff, "C2", "verified invoice"); err == nil {
					mu.Lock()
					won++
					mu.Unlock()
				}
			})
		}
		wg.Wait()
		if won != 1 || len(gw.requests()) != 1 {
			t.Fatalf("approvals won = %d, payment requests = %d; want 1 and 1", won, len(gw.requests()))
		}
	})
}

// The payment runs detached from the caller: a client that disconnects
// mid-payment cannot strand the claim in PENDING.
func TestCallerCancellationDoesNotStrandThePayment(t *testing.T) {
	forEachStore(t, func(t *testing.T, env0 testEnv) {
		ctx, cancel := context.WithCancel(as(app.RoleAgencyStaff, ""))
		defer cancel()
		gw := &fakeGateway{onCall: cancel}
		env := newEnvWith(t, env0.store, gw, nil)
		c, err := env.svc.Claims.Approve(ctx, "C2", "verified invoice")
		if err != nil || c.Status != domain.ClaimPaid {
			t.Fatalf("claim = %+v err %v; the payment must complete after the caller left", c, err)
		}
		stored, _ := env.store.GetClaim(context.Background(), "C2")
		if stored.Status != domain.ClaimPaid {
			t.Fatalf("stored status %s, want PAID", stored.Status)
		}
	})
}

// With payments off, approval is just approval.
func TestApprovalWithoutPayments(t *testing.T) {
	svc, _, st := newServices(t)
	c, err := svc.Claims.Approve(as(app.RoleAgencyStaff, ""), "C2", "verified invoice")
	if err != nil || c.Status != domain.ClaimApproved || c.Payment != nil {
		t.Fatalf("claim = %+v err %v", c, err)
	}
	if stored, _ := st.GetClaim(context.Background(), "C2"); stored.Payment != nil {
		t.Fatalf("no payment may be recorded, got %+v", stored.Payment)
	}
}
