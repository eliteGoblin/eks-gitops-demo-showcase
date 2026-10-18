package app_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/eliteGoblin/eks-gitops-demo/internal/app"
	"github.com/eliteGoblin/eks-gitops-demo/internal/domain"
)

func raiseTherapy(t *testing.T, svc *app.Services, ctx context.Context, planID string, amount domain.Money) domain.SupportRequest {
	t.Helper()
	r, err := svc.SupportRequests.Raise(ctx, app.RaiseSupportRequestInput{
		PlanID: planID, Category: domain.CategoryTherapy, Description: "Weekly physiotherapy", EstimatedAmount: amount,
	})
	if err != nil {
		t.Fatalf("raise: %v", err)
	}
	return r
}

func TestRaiseSupportRequest(t *testing.T) {
	forEachStore(t, func(t *testing.T, env testEnv) {
		svc := env.svc
		r := raiseTherapy(t, svc, as(app.RoleParticipant, "P1"), "PL1", 300_00)
		if r.Status != domain.SupportRequestPending || r.ParticipantID != "P1" || r.RequestedBy != "sub-participant-P1" || r.ID == "" {
			t.Fatalf("unexpected request %+v", r)
		}
		// A nominee raises for the participant they act for; staff for anyone.
		raiseTherapy(t, svc, as(app.RoleNominee, "N1"), "PL1", 10_00)
		raiseTherapy(t, svc, as(app.RoleAgencyStaff, ""), "PL3", 10_00)

		forbidden := []struct {
			name string
			ctx  context.Context
			plan string
		}{
			{"other participant", as(app.RoleParticipant, "P3"), "PL1"},
			{"provider", as(app.RoleProvider, "PR1"), "PL1"},
			{"unknown plan looks forbidden", as(app.RoleParticipant, "P1"), "PL404"},
		}
		for _, tc := range forbidden {
			_, err := svc.SupportRequests.Raise(tc.ctx, app.RaiseSupportRequestInput{PlanID: tc.plan, Category: domain.CategoryTherapy, Description: "x", EstimatedAmount: 1})
			if !errors.Is(err, app.ErrForbidden) {
				t.Errorf("%s: want ErrForbidden, got %v", tc.name, err)
			}
		}
		var ve *domain.ValidationError
		_, err := svc.SupportRequests.Raise(as(app.RoleParticipant, "P1"), app.RaiseSupportRequestInput{
			PlanID: "PL1", Category: domain.CategoryTherapy, Description: "x", EstimatedAmount: 1, PreferredProviderID: "PR404"})
		if !errors.As(err, &ve) || ve.Field != "preferredProviderId" {
			t.Fatalf("unknown preferred provider: %v", err)
		}
	})
}

func TestDecideSupportRequest(t *testing.T) {
	forEachStore(t, func(t *testing.T, env testEnv) {
		svc := env.svc
		staff := as(app.RoleAgencyStaff, "")
		r := raiseTherapy(t, svc, as(app.RoleParticipant, "P1"), "PL1", 300_00)

		if _, err := svc.SupportRequests.Decide(as(app.RoleParticipant, "P1"), app.DecideSupportRequestInput{ID: r.ID, Approve: true, ProviderID: "PR1", Reason: "x"}); !errors.Is(err, app.ErrForbidden) {
			t.Fatalf("only staff decide: %v", err)
		}
		var ve *domain.ValidationError
		if _, err := svc.SupportRequests.Decide(staff, app.DecideSupportRequestInput{ID: "SR404", Approve: true, ProviderID: "PR1", Reason: "x"}); !errors.As(err, &ve) || ve.Field != "id" {
			t.Fatalf("unknown request: %v", err)
		}
		if _, err := svc.SupportRequests.Decide(staff, app.DecideSupportRequestInput{ID: r.ID, Approve: true, ProviderID: "PR404", Reason: "x"}); !errors.As(err, &ve) || ve.Field != "providerId" {
			t.Fatalf("unknown provider: %v", err)
		}

		// Before approval PR1 is not booked for THERAPY on PL1, so it cannot claim.
		provider := as(app.RoleProvider, "PR1")
		therapyClaim := app.SubmitClaimInput{PlanID: "PL1", Category: domain.CategoryTherapy, Amount: 120_00, ServiceDate: "2026-05-01"}
		if _, err := svc.Claims.Submit(provider, therapyClaim); !errors.Is(err, app.ErrForbidden) {
			t.Fatalf("unbooked provider must not claim: %v", err)
		}

		approved, err := svc.SupportRequests.Decide(staff, app.DecideSupportRequestInput{ID: r.ID, Approve: true, ProviderID: "PR1", Reason: "assessed by OT"})
		if err != nil || approved.Status != domain.SupportRequestApproved || approved.BookedProviderID != "PR1" {
			t.Fatalf("approve: %+v %v", approved, err)
		}
		if _, err := svc.SupportRequests.Decide(staff, app.DecideSupportRequestInput{ID: r.ID, Approve: false, Reason: "again"}); !errors.As(err, &ve) {
			t.Fatalf("a decided request cannot be decided again: %v", err)
		}

		// The approval booked PR1 and reserved 300.00 of THERAPY: now it can claim.
		if _, err := svc.Claims.Submit(provider, therapyClaim); err != nil {
			t.Fatalf("booked provider claims against the booking: %v", err)
		}
		plan, _ := svc.Plans.Get(staff, "PL1")
		budgets, err := svc.Plans.Budgets(staff, *plan)
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range budgets {
			if b.Category == domain.CategoryTherapy && (b.Spent != 120_00 || b.Reserved != 180_00) {
				t.Fatalf("THERAPY budget %+v, want spent 120.00 reserved 180.00", b)
			}
		}

		if !env.audit.has(app.ActionDecideSupportRequest, "approved", "assessed by OT") {
			t.Fatal("the approval must be audited with its reason")
		}
	})
}

func TestNobodyDecidesTheirOwnRequest(t *testing.T) {
	forEachStore(t, func(t *testing.T, env testEnv) {
		staff := as(app.RoleAgencyStaff, "")
		r := raiseTherapy(t, env.svc, staff, "PL3", 50_00) // staff raising for a participant
		var self *domain.SelfApprovalNotAllowedError
		for _, approve := range []bool{true, false} {
			if _, err := env.svc.SupportRequests.Decide(staff, app.DecideSupportRequestInput{ID: r.ID, Approve: approve, ProviderID: "PR1", Reason: "ok"}); !errors.As(err, &self) {
				t.Fatalf("approve=%v on own request: want SelfApprovalNotAllowedError, got %v", approve, err)
			}
		}
		other := app.WithPrincipal(context.Background(), app.Principal{Subject: "sub-other-officer", Roles: map[app.Role]bool{app.RoleAgencyStaff: true}})
		if _, err := env.svc.SupportRequests.Decide(other, app.DecideSupportRequestInput{ID: r.ID, Approve: false, Reason: "not eligible"}); err != nil {
			t.Fatalf("another officer may decide: %v", err)
		}
	})
}

func TestApprovalRespectsAvailableBudget(t *testing.T) {
	forEachStore(t, func(t *testing.T, env testEnv) {
		staff := as(app.RoleAgencyStaff, "")
		// PL1 THERAPY: 5000.00 allocated, nothing spent or reserved.
		big := raiseTherapy(t, env.svc, as(app.RoleParticipant, "P1"), "PL1", 4_000_00)
		small := raiseTherapy(t, env.svc, as(app.RoleParticipant, "P1"), "PL1", 1_500_00)
		if _, err := env.svc.SupportRequests.Decide(staff, app.DecideSupportRequestInput{ID: big.ID, Approve: true, ProviderID: "PR2", Reason: "ok"}); err != nil {
			t.Fatal(err)
		}
		var be *domain.BudgetExceededError
		if _, err := env.svc.SupportRequests.Decide(staff, app.DecideSupportRequestInput{ID: small.ID, Approve: true, ProviderID: "PR1", Reason: "ok"}); !errors.As(err, &be) || be.Remaining != 1_000_00 {
			t.Fatalf("want BudgetExceeded with 1000.00 available, got %v", err)
		}
		if got, _ := env.svc.SupportRequests.Pending(staff); len(got) != 1 || got[0].ID != small.ID {
			t.Fatalf("a refused approval leaves the request pending, got %+v", got)
		}
	})
}

// Concurrent approvals reserve atomically: together they never reserve more
// than is available.
func TestConcurrentApprovalsNeverOverReserve(t *testing.T) {
	forEachStore(t, func(t *testing.T, env testEnv) {
		staff := as(app.RoleAgencyStaff, "")
		const n = 20
		ids := make([]string, n)
		for i := range ids {
			ids[i] = raiseTherapy(t, env.svc, as(app.RoleParticipant, "P1"), "PL1", 400_00).ID // 12 of 20 fit in 5000.00
		}
		var wg sync.WaitGroup
		for _, id := range ids {
			wg.Go(func() {
				_, _ = env.svc.SupportRequests.Decide(staff, app.DecideSupportRequestInput{ID: id, Approve: true, ProviderID: "PR2", Reason: "ok"})
			})
		}
		wg.Wait()
		plan, _ := env.svc.Plans.Get(staff, "PL1")
		budgets, _ := env.svc.Plans.Budgets(staff, *plan)
		for _, b := range budgets {
			if b.Category == domain.CategoryTherapy && b.Reserved != 12*400_00 {
				t.Fatalf("reserved %v, want exactly 4800.00", b.Reserved)
			}
		}
	})
}

func TestSupportRequestListings(t *testing.T) {
	forEachStore(t, func(t *testing.T, env testEnv) {
		svc := env.svc
		staff := as(app.RoleAgencyStaff, "")
		own := raiseTherapy(t, svc, as(app.RoleParticipant, "P1"), "PL1", 10_00)
		byStaff := raiseTherapy(t, svc, staff, "PL3", 10_00)

		mine, err := svc.SupportRequests.Mine(as(app.RoleParticipant, "P1"))
		if err != nil || len(mine) != 1 || mine[0].ID != own.ID {
			t.Fatalf("participant sees requests about them: %+v %v", mine, err)
		}
		if got, _ := svc.SupportRequests.Mine(as(app.RoleNominee, "N1")); len(got) != 1 || got[0].ID != own.ID {
			t.Fatalf("nominee sees the requests of the participant they act for: %+v", got)
		}
		if got, _ := svc.SupportRequests.Mine(as(app.RoleParticipant, "P3")); len(got) != 1 || got[0].ID != byStaff.ID {
			t.Fatalf("P3 sees the request staff raised for them: %+v", got)
		}
		if got, _ := svc.SupportRequests.Mine(staff); len(got) != 1 || got[0].ID != byStaff.ID {
			t.Fatalf("staff 'mine' is what they raised, not everything: %+v", got)
		}
		if got, err := svc.SupportRequests.Pending(staff); err != nil || len(got) != 2 {
			t.Fatalf("staff see every pending request: %+v %v", got, err)
		}
		if _, err := svc.SupportRequests.Pending(as(app.RoleParticipant, "P1")); !errors.Is(err, app.ErrForbidden) {
			t.Fatalf("pending queue is staff-only: %v", err)
		}
	})
}

// A reservation approved after the provider over-claimed an earlier one is
// held in full: claims beyond the first reservation do not eat into the
// second, so another provider cannot take it.
func TestReservationAfterAnOverClaimIsHeldInFull(t *testing.T) {
	forEachStore(t, func(t *testing.T, env testEnv) {
		staff, p1 := as(app.RoleAgencyStaff, ""), as(app.RoleParticipant, "P1")
		approve := func(r domain.SupportRequest) {
			t.Helper()
			if _, err := env.svc.SupportRequests.Decide(staff, app.DecideSupportRequestInput{ID: r.ID, Approve: true, Reason: "assessed"}); err != nil {
				t.Fatalf("approve %s: %v", r.ID, err)
			}
		}
		claim := func(provider string, amount domain.Money) error {
			_, err := env.svc.Claims.Submit(as(app.RoleProvider, provider), app.SubmitClaimInput{
				PlanID: "PL1", Category: domain.CategoryTherapy, Amount: amount, ServiceDate: "2026-05-01"})
			return err
		}
		// PL1 THERAPY: 5000.00 allocated, nothing spent or reserved yet.
		approve(raiseTherapyWithProvider(t, env.svc, p1, "PL1", 100_00, "PR1"))
		if err := claim("PR1", 150_00); err != nil {
			t.Fatalf("PR1 may claim beyond its reservation while budget is unreserved: %v", err)
		}
		approve(raiseTherapyWithProvider(t, env.svc, p1, "PL1", 80_00, "PR1"))
		approve(raiseTherapyWithProvider(t, env.svc, p1, "PL1", 10_00, "PR2")) // PR2 needs a booking to claim

		// 5000.00 - 150.00 spent - 80.00 held for PR1 = 4770.00 for PR2 (its own 10.00 included).
		var be *domain.BudgetExceededError
		if err := claim("PR2", 4_770_01); !errors.As(err, &be) || be.Remaining != 4_770_00 {
			t.Fatalf("PR1's new 80.00 must stay held, got %v", err)
		}
		if err := claim("PR2", 4_770_00); err != nil {
			t.Fatalf("PR2 may claim everything not held for PR1: %v", err)
		}
		if err := claim("PR1", 80_00); err != nil {
			t.Fatalf("PR1's second reservation is intact: %v", err)
		}
	})
}

// Claims and reservations racing on one plan never commit more than the
// allocation: what is spent plus what is still reserved stays within it.
func TestConcurrentClaimsAndApprovalsStayWithinTheAllocation(t *testing.T) {
	forEachStore(t, func(t *testing.T, env testEnv) {
		staff, p1 := as(app.RoleAgencyStaff, ""), as(app.RoleParticipant, "P1")
		first := raiseTherapyWithProvider(t, env.svc, p1, "PL1", 100_00, "PR1") // PR1 needs a booking to claim
		if _, err := env.svc.SupportRequests.Decide(staff, app.DecideSupportRequestInput{ID: first.ID, Approve: true, Reason: "ok"}); err != nil {
			t.Fatal(err)
		}
		const approvals, claims = 10, 30 // 10 x 600.00 + 30 x 200.00 asked of 4900.00 unreserved
		ids := make([]string, approvals)
		for i := range ids {
			ids[i] = raiseTherapyWithProvider(t, env.svc, p1, "PL1", 600_00, "PR2").ID
		}
		var wg sync.WaitGroup
		for _, id := range ids {
			wg.Go(func() {
				_, _ = env.svc.SupportRequests.Decide(staff, app.DecideSupportRequestInput{ID: id, Approve: true, Reason: "ok"})
			})
		}
		for range claims {
			wg.Go(func() {
				_, _ = env.svc.Claims.Submit(as(app.RoleProvider, "PR1"), app.SubmitClaimInput{
					PlanID: "PL1", Category: domain.CategoryTherapy, Amount: 200_00, ServiceDate: "2026-05-01"})
			})
		}
		wg.Wait()

		ctx := context.Background()
		planClaims, err := env.store.ClaimsOfPlan(ctx, "PL1")
		if err != nil {
			t.Fatal(err)
		}
		bookings, err := env.store.BookingsOfPlan(ctx, "PL1")
		if err != nil {
			t.Fatal(err)
		}
		spent := domain.SpentIn(planClaims, domain.CategoryTherapy)
		reserved := domain.OutstandingReserved(planClaims, bookings, domain.CategoryTherapy)
		if spent+reserved > 5_000_00 {
			t.Fatalf("spent %v + reserved %v exceeds the 5000.00 allocation", spent, reserved)
		}
		if spent+reserved < 4_900_00 {
			t.Fatalf("spent %v + reserved %v: the race left budget unused, so nothing was contended", spent, reserved)
		}
	})
}
