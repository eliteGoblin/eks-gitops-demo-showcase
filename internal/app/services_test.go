package app_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eliteGoblin/eks-gitops-demo/internal/app"
	"github.com/eliteGoblin/eks-gitops-demo/internal/domain"
)

// as returns a context for a caller with one role; each (role, id) pair is a
// distinct person with its own subject.
func as(role app.Role, id string) context.Context {
	subject := "sub-" + string(role)
	if id != "" {
		subject += "-" + id
	}
	p := app.Principal{Subject: subject, Username: string(role), Roles: map[app.Role]bool{role: true}}
	switch role {
	case app.RoleParticipant:
		p.ParticipantID = id
	case app.RoleNominee:
		p.NomineeID = id
	case app.RoleProvider:
		p.ProviderID = id
	}
	return app.WithPrincipal(context.Background(), p)
}

func TestNewServicesRequiresDeps(t *testing.T) {
	_, err := app.NewServices(app.Deps{})
	if err == nil || !strings.Contains(err.Error(), "Participants") {
		t.Fatalf("expected missing-dependency error, got %v", err)
	}
}

// As a provider: claims -> plan -> participant must show only booked plans and mask dateOfBirth.
func TestProviderTraversalMasksDOBAndFiltersPlans(t *testing.T) {
	svc, _, _ := newServices(t)
	ctx := as(app.RoleProvider, "PR1")

	page, err := svc.Claims.List(ctx, app.ClaimFilter{}, app.Page{First: 20})
	if err != nil || len(page.Claims) != 2 {
		t.Fatalf("list claims: %v (n=%d), want 2 PR1 claims", err, len(page.Claims))
	}
	for _, c := range page.Claims {
		plan, err := svc.Plans.Get(ctx, c.PlanID)
		if err != nil || plan == nil || plan.ID != "PL1" {
			t.Fatalf("claim %s: plan %v err %v; provider must only reach PL1", c.ID, plan, err)
		}
		p, err := svc.Participants.Get(ctx, plan.ParticipantID)
		if err != nil || p == nil || p.DateOfBirth != nil {
			t.Fatalf("participant must be visible with masked DOB, got %+v err %v", p, err)
		}
		budgets, err := svc.Plans.Budgets(ctx, *plan)
		if err != nil || len(budgets) != 1 || budgets[0].Category != domain.CategoryDailyLiving {
			t.Fatalf("provider must see only booked DAILY_LIVING budget, got %+v err %v", budgets, err)
		}
	}
	if p, err := svc.Participants.Get(ctx, "P2"); err != nil || p != nil {
		t.Fatalf("participant(P2) must be nil for provider, got %v err %v", p, err)
	}
	if pl, err := svc.Plans.Get(ctx, "PL2"); err != nil || pl != nil {
		t.Fatalf("plan(PL2) must be nil for provider, got %v err %v", pl, err)
	}
}

func TestParticipantAndNomineeReads(t *testing.T) {
	svc, audit, _ := newServices(t)
	for _, ctx := range []context.Context{as(app.RoleParticipant, "P1"), as(app.RoleNominee, "N1")} {
		p, err := svc.Participants.Get(ctx, "P1")
		if err != nil || p == nil || p.DateOfBirth == nil || *p.DateOfBirth != "1990-05-04" {
			t.Fatalf("own/delegated participant must show DOB, got %+v err %v", p, err)
		}
		plans, err := svc.Plans.ListByParticipant(ctx, "P1")
		if err != nil || len(plans) != 1 {
			t.Fatalf("plans: %v err %v", plans, err)
		}
		budgets, err := svc.Plans.Budgets(ctx, plans[0])
		if err != nil || len(budgets) != 2 {
			t.Fatalf("budgets: %v err %v", budgets, err)
		}
	}
	guest := as(app.RoleParticipant, "P3")
	if p, err := svc.Participants.Get(guest, "P1"); err != nil || p != nil {
		t.Fatalf("other participant must be hidden, got %v err %v", p, err)
	}
	if plans, err := svc.Plans.ListByParticipant(guest, "P1"); err != nil || len(plans) != 0 {
		t.Fatalf("other participant's plans must be filtered, got %v err %v", plans, err)
	}
	if p, err := svc.Participants.Get(as(app.RoleAgencyStaff, ""), "P404"); err != nil || p != nil {
		t.Fatalf("missing participant must be nil, got %v err %v", p, err)
	}
	if len(audit.events) == 0 {
		t.Fatal("reads must be audited")
	}
	denied := 0
	for _, e := range audit.events {
		if e.Outcome == "deny" {
			denied++
		}
	}
	if denied == 0 {
		t.Fatal("denied reads must be audited as deny")
	}
}

func TestUnauthenticatedCallsFail(t *testing.T) {
	svc, _, _ := newServices(t)
	ctx := context.Background()
	if _, err := svc.Claims.Submit(ctx, app.SubmitClaimInput{PlanID: "PL1", Category: domain.CategoryDailyLiving, Amount: 1}); !errors.Is(err, app.ErrUnauthenticated) {
		t.Fatalf("submit without principal: %v", err)
	}
	if _, err := svc.Plans.Budgets(ctx, domain.Plan{ID: "PL1", ParticipantID: "P1"}); !errors.Is(err, app.ErrUnauthenticated) {
		t.Fatalf("budgets without principal: %v", err)
	}
}

func TestSubmitClaim(t *testing.T) {
	svc, _, st := newServices(t)
	provider := as(app.RoleProvider, "PR1")

	_, err := svc.Claims.Submit(provider, app.SubmitClaimInput{PlanID: "PL1", Category: domain.CategoryDailyLiving, Amount: 1_000_000_00, ServiceDate: "2026-05-01"})
	var over *domain.BudgetExceededError
	// seed: allocated 10000.00, spent 250.00 + 180.50
	if !errors.As(err, &over) || over.Remaining != 9_569_50 {
		t.Fatalf("expected BudgetExceeded with remaining 9569.50, got %v", err)
	}

	c, err := svc.Claims.Submit(provider, app.SubmitClaimInput{PlanID: "PL1", Category: domain.CategoryDailyLiving, Amount: 100_00, ServiceDate: "2026-05-01"})
	if err != nil || c.Status != domain.ClaimSubmitted || c.ProviderID != "PR1" || c.ID == "" {
		t.Fatalf("expected a new SUBMITTED claim, got %+v err %v", c, err)
	}
	if stored, err := st.GetClaim(context.Background(), c.ID); err != nil || stored.Amount != 100_00 {
		t.Fatalf("claim not stored: %+v %v", stored, err)
	}

	var ve *domain.ValidationError
	if _, err := svc.Claims.Submit(provider, app.SubmitClaimInput{PlanID: "PL1", Category: domain.CategoryDailyLiving, Amount: 0}); !errors.As(err, &ve) || ve.Field != "amount" {
		t.Fatalf("zero amount: %v", err)
	}
	if _, err := svc.Claims.Submit(provider, app.SubmitClaimInput{PlanID: "PL1", Category: domain.CategoryTherapy, Amount: 100}); !errors.Is(err, app.ErrForbidden) {
		t.Fatalf("unbooked category must be forbidden: %v", err)
	}
	if _, err := svc.Claims.Submit(provider, app.SubmitClaimInput{PlanID: "PL404", Category: domain.CategoryDailyLiving, Amount: 100}); !errors.Is(err, app.ErrForbidden) {
		t.Fatalf("unknown plan must look forbidden: %v", err)
	}
	if _, err := svc.Claims.Submit(as(app.RoleParticipant, "P1"), app.SubmitClaimInput{PlanID: "PL1", Category: domain.CategoryDailyLiving, Amount: 100}); !errors.Is(err, app.ErrForbidden) {
		t.Fatalf("participant must not submit: %v", err)
	}
}

func TestApproveClaim(t *testing.T) {
	svc, audit, _ := newServices(t)
	staff := as(app.RoleAgencyStaff, "")
	var ve *domain.ValidationError

	if _, err := svc.Claims.Approve(staff, "C2", "  "); !errors.As(err, &ve) || ve.Field != "reason" {
		t.Fatalf("blank reason: %v", err)
	}
	if _, err := svc.Claims.Approve(staff, "C404", "ok"); !errors.As(err, &ve) || ve.Message != "claim not found" {
		t.Fatalf("missing claim: %v", err)
	}
	// Callers who may not approve learn nothing about which claims exist.
	if _, err := svc.Claims.Approve(as(app.RoleProvider, "PR1"), "C404", "ok"); !errors.Is(err, app.ErrForbidden) {
		t.Fatalf("missing claim must look forbidden to a non-approver: %v", err)
	}
	if _, err := svc.Claims.Approve(as(app.RoleProvider, "PR1"), "C2", "ok"); !errors.Is(err, app.ErrForbidden) {
		t.Fatalf("provider must not approve: %v", err)
	}
	c, err := svc.Claims.Approve(staff, "C2", "verified invoice")
	if err != nil || c.Status != domain.ClaimApproved {
		t.Fatalf("approve: %+v %v", c, err)
	}
	if _, err := svc.Claims.Approve(staff, "C2", "again"); !errors.As(err, &ve) {
		t.Fatalf("approving twice must fail: %v", err)
	}
	last := audit.events[len(audit.events)-2] // the failed second approve adds only its authorization event
	if last.Outcome != "approved" || last.Detail != "verified invoice" {
		t.Fatalf("approval must be audited with its reason, got %+v", last)
	}

	status := domain.ClaimApproved
	page, err := svc.Claims.List(staff, app.ClaimFilter{Status: &status}, app.Page{First: 20})
	if err != nil || len(page.Claims) != 2 || page.Total != 2 {
		t.Fatalf("approved filter: %+v err %v", page, err)
	}
}

func TestProviderService(t *testing.T) {
	svc, _, _ := newServices(t)
	if _, err := svc.Providers.Get(context.Background(), "PR1"); !errors.Is(err, app.ErrUnauthenticated) {
		t.Fatalf("provider lookup without a principal: %v", err)
	}
	ctx := as(app.RoleParticipant, "P1")
	if p, err := svc.Providers.Get(ctx, "PR1"); err != nil || p == nil || p.Name != "Sunrise Care" {
		t.Fatalf("provider: %v %v", p, err)
	}
	if p, err := svc.Providers.Get(ctx, "PR404"); err != nil || p != nil {
		t.Fatalf("missing provider must be nil: %v %v", p, err)
	}
}

// The budget check and the insert are one atomic step: concurrent submissions
// can never spend more than the allocation.
func TestConcurrentSubmitsNeverExceedBudget(t *testing.T) {
	forEachStore(t, func(t *testing.T, env testEnv) {
		provider := as(app.RoleProvider, "PR1")
		const (
			workers = 100
			amount  = domain.Money(200_00) // seed leaves 9569.50 of DAILY_LIVING on PL1, so 47 fit
		)
		var wg sync.WaitGroup
		for range workers {
			wg.Go(func() {
				_, _ = env.svc.Claims.Submit(provider, app.SubmitClaimInput{PlanID: "PL1", Category: domain.CategoryDailyLiving, Amount: amount, ServiceDate: "2026-05-01"})
			})
		}
		wg.Wait()

		claims, err := env.store.ClaimsOfPlan(context.Background(), "PL1")
		if err != nil {
			t.Fatal(err)
		}
		if spent := domain.SpentIn(claims, domain.CategoryDailyLiving); spent > 10_000_00 {
			t.Fatalf("spent %v exceeds the 10000.00 allocation", spent)
		}
		if accepted := len(claims) - 2; accepted != 47 { // 2 seeded claims
			t.Fatalf("accepted %d claims, want exactly 47", accepted)
		}
	})
}

// Listing is scoped by the policy, paged by keyset and audited once.
func TestListClaimsPagesByKeyset(t *testing.T) {
	forEachStore(t, func(t *testing.T, env testEnv) {
		staff := as(app.RoleAgencyStaff, "")
		for range 5 {
			if _, err := env.svc.Claims.Submit(as(app.RoleProvider, "PR1"), app.SubmitClaimInput{PlanID: "PL1", Category: domain.CategoryDailyLiving, Amount: 1_00, ServiceDate: "2026-05-01"}); err != nil {
				t.Fatal(err)
			}
		}
		env.audit.reset()
		first, err := env.svc.Claims.List(staff, app.ClaimFilter{}, app.Page{First: 3})
		if err != nil || len(first.Claims) != 3 || !first.HasNext || first.Total != 8 {
			t.Fatalf("page 1: %+v %v", first, err)
		}
		if n := len(env.audit.events); n != 1 || env.audit.events[0].Detail != "3 of 8 claims" {
			t.Fatalf("a list is audited once, got %d events %+v", n, env.audit.events)
		}
		var seen []string
		page := first
		for {
			for _, c := range page.Claims {
				seen = append(seen, c.ID)
			}
			if !page.HasNext {
				break
			}
			last := page.Claims[len(page.Claims)-1]
			page, err = env.svc.Claims.List(staff, app.ClaimFilter{}, app.Page{First: 3, After: &app.ClaimCursor{SubmittedAt: last.SubmittedAt, ID: last.ID}})
			if err != nil {
				t.Fatal(err)
			}
		}
		if len(seen) != 8 || seen[0] != "C1" {
			t.Fatalf("pages must cover every claim once in order, got %v", seen)
		}

		// Scope: a provider lists only its own claims; a participant only theirs.
		if got, _ := env.svc.Claims.List(as(app.RoleProvider, "PR2"), app.ClaimFilter{}, app.Page{First: 20}); got.Total != 1 || got.Claims[0].ID != "C3" {
			t.Fatalf("PR2 scope: %+v", got)
		}
		if got, _ := env.svc.Claims.List(as(app.RoleParticipant, "P3"), app.ClaimFilter{}, app.Page{First: 20}); got.Total != 0 {
			t.Fatalf("P3 has no claims: %+v", got)
		}
		if got, _ := env.svc.Claims.List(as(app.RoleNominee, "N1"), app.ClaimFilter{PlanID: "PL2"}, app.Page{First: 20}); got.Total != 0 {
			t.Fatalf("nominee for P1 must not list PL2: %+v", got)
		}
	})
}

func TestStaleOrForeignCursorIsRejected(t *testing.T) {
	forEachStore(t, func(t *testing.T, env testEnv) {
		c3, err := env.store.GetClaim(context.Background(), "C3") // PR2's claim on PL2
		if err != nil {
			t.Fatal(err)
		}
		tests := []struct {
			name   string
			cursor app.ClaimCursor
		}{
			{"unknown claim", app.ClaimCursor{SubmittedAt: c3.SubmittedAt, ID: "C404"}},
			{"time does not match", app.ClaimCursor{SubmittedAt: c3.SubmittedAt.Add(time.Second), ID: "C3"}},
			{"claim outside the caller's scope", app.ClaimCursor{SubmittedAt: c3.SubmittedAt, ID: "C3"}},
		}
		for _, tc := range tests {
			_, err := env.svc.Claims.List(as(app.RoleProvider, "PR1"), app.ClaimFilter{}, app.Page{First: 2, After: &tc.cursor})
			if !errors.Is(err, domain.ErrInvalidInput) {
				t.Errorf("%s: want ErrInvalidInput, got %v", tc.name, err)
			}
		}
		if _, err := env.svc.Claims.List(as(app.RoleProvider, "PR1"), app.ClaimFilter{}, app.Page{First: 0}); !errors.Is(err, domain.ErrInvalidInput) {
			t.Errorf("first must be at least 1: %v", err)
		}
	})
}

// Batch reads (for DataLoaders) apply the same policy and masking as single reads.
func TestBatchReads(t *testing.T) {
	forEachStore(t, func(t *testing.T, env testEnv) {
		provider := as(app.RoleProvider, "PR1")
		plans, err := env.svc.Plans.GetMany(provider, []string{"PL1", "PL2", "PL404"})
		if err != nil || len(plans) != 3 || plans[0] == nil || plans[0].ID != "PL1" || plans[1] != nil || plans[2] != nil {
			t.Fatalf("plans aligned with keys, hidden ones nil: %+v %v", plans, err)
		}
		people, err := env.svc.Participants.GetMany(provider, []string{"P1", "P2"})
		if err != nil || people[0] == nil || people[0].DateOfBirth != nil || people[1] != nil {
			t.Fatalf("participants: masked DOB for P1, P2 hidden: %+v %v", people, err)
		}
		provs, err := env.svc.Providers.GetMany(provider, []string{"PR2", "PR404"})
		if err != nil || provs[0] == nil || provs[0].Name != "Harbour Therapy" || provs[1] != nil {
			t.Fatalf("providers: %+v %v", provs, err)
		}
		if _, err := env.svc.Providers.GetMany(context.Background(), []string{"PR1"}); !errors.Is(err, app.ErrUnauthenticated) {
			t.Fatalf("batch reads need a principal: %v", err)
		}
	})
}

// Reservations made for one provider are not budget another provider may
// claim: the budget check sees the plan's bookings, under the same lock.
func TestProviderCannotClaimAnotherProvidersReservation(t *testing.T) {
	forEachStore(t, func(t *testing.T, env testEnv) {
		staff := as(app.RoleAgencyStaff, "")
		// PL1 DAILY_LIVING: 10000.00 allocated, 430.50 spent by PR1, PR1 reserved 1000.00 (569.50 outstanding).
		r, err := env.svc.SupportRequests.Raise(as(app.RoleParticipant, "P1"), app.RaiseSupportRequestInput{
			PlanID: "PL1", Category: domain.CategoryDailyLiving, Description: "Home support", EstimatedAmount: 9_000_00, PreferredProviderID: "PR2"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := env.svc.SupportRequests.Decide(staff, app.DecideSupportRequestInput{ID: r.ID, Approve: true, Reason: "assessed"}); err != nil {
			t.Fatalf("PR2's 9000.00 reservation fits exactly: %v", err)
		}
		pr1 := as(app.RoleProvider, "PR1")
		var be *domain.BudgetExceededError
		_, err = env.svc.Claims.Submit(pr1, app.SubmitClaimInput{PlanID: "PL1", Category: domain.CategoryDailyLiving, Amount: 1_000_00, ServiceDate: "2026-05-01"})
		if !errors.As(err, &be) || be.Remaining != 569_50 {
			t.Fatalf("PR1 may only claim its own outstanding 569.50, got %v", err)
		}
		if _, err := env.svc.Claims.Submit(pr1, app.SubmitClaimInput{PlanID: "PL1", Category: domain.CategoryDailyLiving, Amount: 569_50, ServiceDate: "2026-05-01"}); err != nil {
			t.Fatalf("PR1's own reservation is claimable: %v", err)
		}
		if _, err := env.svc.Claims.Submit(as(app.RoleProvider, "PR2"), app.SubmitClaimInput{PlanID: "PL1", Category: domain.CategoryDailyLiving, Amount: 9_000_00, ServiceDate: "2026-05-01"}); err != nil {
			t.Fatalf("PR2's reservation is still intact: %v", err)
		}
	})
}

// Two staff approving the same claim at once: exactly one approval is stored
// and audited; the other sees the claim is no longer SUBMITTED.
func TestApproveClaimRunsOnce(t *testing.T) {
	forEachStore(t, func(t *testing.T, env testEnv) {
		c, err := env.svc.Claims.Submit(as(app.RoleProvider, "PR1"), app.SubmitClaimInput{PlanID: "PL1", Category: domain.CategoryDailyLiving, Amount: 1_00, ServiceDate: "2026-05-01"})
		if err != nil {
			t.Fatal(err)
		}
		env.audit.reset()
		var wg sync.WaitGroup
		var mu sync.Mutex
		approved, refused := 0, 0
		for range 10 {
			wg.Go(func() {
				_, err := env.svc.Claims.Approve(as(app.RoleAgencyStaff, ""), c.ID, "verified")
				var ve *domain.ValidationError
				mu.Lock()
				defer mu.Unlock()
				switch {
				case err == nil:
					approved++
				case errors.As(err, &ve) && ve.Field == "id":
					refused++
				default:
					t.Errorf("unexpected error: %v", err)
				}
			})
		}
		wg.Wait()
		if approved != 1 || refused != 9 {
			t.Fatalf("approved %d refused %d, want 1 and 9", approved, refused)
		}
		var events int
		for _, e := range env.audit.events {
			if e.Outcome == "approved" {
				events++
			}
		}
		if events != 1 {
			t.Fatalf("want exactly one 'approved' audit event, got %d", events)
		}
	})
}
