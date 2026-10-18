package postgres_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/eliteGoblin/eks-gitops-demo/internal/adapter/postgres/pgtest"
	"github.com/eliteGoblin/eks-gitops-demo/internal/app"
	"github.com/eliteGoblin/eks-gitops-demo/internal/domain"
)

// The behaviour shared with the memory store is covered by the contract tests
// in internal/app (forEachStore). These cover what is specific to SQL.

func TestNotFoundMapsToDomainError(t *testing.T) {
	st := pgtest.NewStore(t)
	ctx := context.Background()
	checks := map[string]error{}
	_, checks["participant"] = st.GetParticipant(ctx, "P404")
	_, checks["plan"] = st.GetPlan(ctx, "PL404")
	_, checks["provider"] = st.GetProvider(ctx, "PR404")
	_, checks["claim"] = st.GetClaim(ctx, "C404")
	_, checks["support request"] = st.GetSupportRequest(ctx, "SR404")
	checks["update claim"] = st.UpdateClaim(ctx, domain.Claim{ID: "C404", Status: domain.ClaimApproved}, domain.ClaimSubmitted)
	checks["insert on unknown plan"] = st.InsertClaimWithinBudget(ctx, domain.Claim{ID: "X", PlanID: "PL404"}, func([]domain.Claim, []domain.ServiceBooking) error { return nil })
	for name, err := range checks {
		if !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("%s: want domain.ErrNotFound, got %v", name, err)
		}
	}
}

func TestSeedMatchesTheMemoryStore(t *testing.T) {
	st := pgtest.NewStore(t)
	ctx := context.Background()
	plan, err := st.GetPlan(ctx, "PL1")
	if err != nil || plan.ParticipantID != "P1" || len(plan.Allocations) != 2 || plan.StartDate != "2026-01-01" {
		t.Fatalf("PL1: %+v %v", plan, err)
	}
	p, err := st.GetParticipant(ctx, "P1")
	if err != nil || p.DateOfBirth == nil || *p.DateOfBirth != "1990-05-04" {
		t.Fatalf("P1: %+v %v", p, err)
	}
	c1, err := st.GetClaim(ctx, "C1")
	if err != nil || !c1.SubmittedAt.Equal(time.Date(2026, 2, 11, 9, 0, 0, 0, time.UTC)) || c1.ServiceDate != "2026-02-10" {
		t.Fatalf("C1: %+v %v", c1, err)
	}
	bookings, err := st.BookingsOfPlan(ctx, "PL1")
	if err != nil || len(bookings) != 1 || bookings[0].Reserved != 1_000_00 {
		t.Fatalf("bookings: %+v %v", bookings, err)
	}
}

func TestDecidingTwiceLosesTheRace(t *testing.T) {
	st := pgtest.NewStore(t)
	ctx := context.Background()
	r := domain.SupportRequest{ID: "SR1", PlanID: "PL1", ParticipantID: "P1", RequestedBy: "a", RequestedByName: "a",
		Category: domain.CategoryTherapy, Description: "x", EstimatedAmount: 1_00, Status: domain.SupportRequestPending,
		RaisedAt: time.Now()}
	if err := st.InsertSupportRequest(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := r.Reject("b", "no", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := st.DecideSupportRequest(ctx, r, nil); err != nil {
		t.Fatal(err)
	}
	var ve *domain.ValidationError
	if err := st.DecideSupportRequest(ctx, r, nil); !errors.As(err, &ve) || ve.Field != "id" {
		t.Fatalf("second decision must fail: %v", err)
	}
	got, err := st.GetSupportRequest(ctx, "SR1")
	if err != nil || got.Status != domain.SupportRequestRejected || got.DecisionReason != "no" || got.DecidedAt == nil {
		t.Fatalf("stored decision: %+v %v", got, err)
	}
	all, err := st.ListSupportRequests(ctx, app.SupportRequestQuery{Scope: app.Scope{All: true}})
	if err != nil || len(all) != 1 {
		t.Fatalf("list: %+v %v", all, err)
	}
}

// A NUL byte or invalid UTF-8 in an id or filter is the client's mistake:
// BAD_USER_INPUT, never an internal error.
func TestMalformedValuesAreInvalidInput(t *testing.T) {
	st := pgtest.NewStore(t)
	ctx := context.Background()
	checks := map[string]error{}
	_, checks["participant id"] = st.GetParticipant(ctx, "a\x00b")
	_, checks["claim id"] = st.GetClaim(ctx, "\xff")
	_, checks["claims filter"] = st.QueryClaims(ctx, app.ClaimQuery{Scope: app.Scope{All: true}, Filter: app.ClaimFilter{ProviderID: "a\x00b"}})
	_, checks["search term"] = st.SearchProviders(ctx, "su\x00n", 5)
	for name, err := range checks {
		if !errors.Is(err, domain.ErrInvalidInput) {
			t.Errorf("%s: want domain.ErrInvalidInput, got %v", name, err)
		}
		if err != nil && (strings.Contains(err.Error(), "SQLSTATE") || strings.Contains(err.Error(), "ERROR:")) {
			t.Errorf("%s: the error text must not carry the database's message (it can quote the value): %q", name, err.Error())
		}
	}
}
