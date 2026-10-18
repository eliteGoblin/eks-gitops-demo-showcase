package domain

import (
	"errors"
	"strings"
	"testing"
	"time"
)

var raisedAt = time.Date(2026, 5, 2, 9, 0, 0, 0, time.UTC)

func pendingRequest(t *testing.T) SupportRequest {
	t.Helper()
	r, err := NewSupportRequest(SupportRequestDraft{
		ID: "SR1", Plan: activePlan(), RequestedBy: "sub-zhaolin", RequestedByName: "zhaolin.sun",
		Category: CategoryTherapy, Description: "Weekly physiotherapy", EstimatedAmount: 300_00,
	}, raisedAt)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestNewSupportRequest(t *testing.T) {
	r := pendingRequest(t)
	if r.Status != SupportRequestPending || r.PlanID != "PL1" || !r.RaisedAt.Equal(raisedAt) {
		t.Fatalf("unexpected request %+v", r)
	}

	inactive := activePlan()
	inactive.Status = PlanExpired
	tests := []struct {
		name      string
		draft     SupportRequestDraft
		wantField string
	}{
		{"zero amount", SupportRequestDraft{Plan: activePlan(), Category: CategoryTherapy, Description: "x", EstimatedAmount: 0}, "estimatedAmount"},
		{"blank description", SupportRequestDraft{Plan: activePlan(), Category: CategoryTherapy, Description: "  ", EstimatedAmount: 1}, "description"},
		{"long description", SupportRequestDraft{Plan: activePlan(), Category: CategoryTherapy, Description: strings.Repeat("a", 501), EstimatedAmount: 1}, "description"},
		{"no allocation", SupportRequestDraft{Plan: activePlan(), Category: CategoryEquipment, Description: "x", EstimatedAmount: 1}, "category"},
		{"inactive plan", SupportRequestDraft{Plan: inactive, Category: CategoryTherapy, Description: "x", EstimatedAmount: 1}, "planId"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewSupportRequest(tc.draft, raisedAt)
			var ve *ValidationError
			if !errors.As(err, &ve) || ve.Field != tc.wantField {
				t.Fatalf("want ValidationError on %q, got %v", tc.wantField, err)
			}
		})
	}
}

func TestSupportRequestApprove(t *testing.T) {
	decidedAt := raisedAt.Add(time.Hour)

	r := pendingRequest(t)
	r.PreferredProviderID = "PR2"
	if err := r.Approve("sub-officer", "", "assessed", decidedAt); err != nil {
		t.Fatal(err)
	}
	if r.Status != SupportRequestApproved || r.BookedProviderID != "PR2" || r.DecidedBy != "sub-officer" || r.DecidedAt == nil || !r.DecidedAt.Equal(decidedAt) {
		t.Fatalf("approve must book the preferred provider and record the decision, got %+v", r)
	}

	explicit := pendingRequest(t)
	explicit.PreferredProviderID = "PR2"
	if err := explicit.Approve("sub-officer", "PR1", "closer provider", decidedAt); err != nil || explicit.BookedProviderID != "PR1" {
		t.Fatalf("an explicit provider overrides the preference: %+v %v", explicit, err)
	}

	tests := []struct {
		name      string
		mutate    func(*SupportRequest)
		decider   string
		provider  string
		reason    string
		wantField string
		wantSelf  bool
	}{
		{"own request", nil, "sub-zhaolin", "PR1", "ok", "", true},
		{"no provider", nil, "sub-officer", "", "ok", "providerId", false},
		{"blank reason", nil, "sub-officer", "PR1", " ", "reason", false},
		{"already decided", func(r *SupportRequest) { r.Status = SupportRequestRejected }, "sub-officer", "PR1", "ok", "id", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := pendingRequest(t)
			if tc.mutate != nil {
				tc.mutate(&r)
			}
			err := r.Approve(tc.decider, tc.provider, tc.reason, decidedAt)
			var self *SelfApprovalNotAllowedError
			var ve *ValidationError
			switch {
			case tc.wantSelf:
				if !errors.As(err, &self) {
					t.Fatalf("want SelfApprovalNotAllowedError, got %v", err)
				}
			case !errors.As(err, &ve) || ve.Field != tc.wantField:
				t.Fatalf("want ValidationError on %q, got %v", tc.wantField, err)
			}
			if r.Status == SupportRequestApproved {
				t.Fatal("a refused approval must not change the status")
			}
		})
	}
}

func TestSupportRequestReject(t *testing.T) {
	r := pendingRequest(t)
	var self *SelfApprovalNotAllowedError
	if err := r.Reject("sub-zhaolin", "no", raisedAt); !errors.As(err, &self) {
		t.Fatalf("rejecting your own request is also a decision on it: %v", err)
	}
	if err := r.Reject("sub-officer", "not eligible", raisedAt); err != nil || r.Status != SupportRequestRejected || r.DecisionReason != "not eligible" {
		t.Fatalf("reject: %+v %v", r, err)
	}
	if err := r.Reject("sub-officer", "again", raisedAt); err == nil {
		t.Fatal("a decided request cannot be decided again")
	}
}

func TestCheckReservation(t *testing.T) {
	plan := activePlan() // THERAPY 500.00
	claims := []Claim{
		{ProviderID: "PR1", Category: CategoryTherapy, Amount: 100_00, Status: ClaimApproved},
		{ProviderID: "PR1", Category: CategoryTherapy, Amount: 50_00, Status: ClaimRejected},
	}
	bookings := []ServiceBooking{
		{ProviderID: "PR1", PlanID: "PL1", Category: CategoryTherapy, Reserved: 150_00}, // 100 claimed -> 50 outstanding
		{ProviderID: "PR2", PlanID: "PL1", Category: CategoryTherapy, Reserved: 200_00}, // nothing claimed -> 200 outstanding
		{ProviderID: "PR2", PlanID: "PL1", Category: CategoryDailyLiving, Reserved: 900_00},
	}
	if got := OutstandingReserved(claims, bookings, CategoryTherapy); got != 250_00 {
		t.Fatalf("outstanding reserved = %v, want 250.00", got)
	}
	// available = 500 - 100 spent - 250 reserved = 150
	if err := plan.CheckReservation(CategoryTherapy, 150_00, claims, bookings); err != nil {
		t.Fatalf("150.00 fits: %v", err)
	}
	var be *BudgetExceededError
	if err := plan.CheckReservation(CategoryTherapy, 150_01, claims, bookings); !errors.As(err, &be) || be.Remaining != 150_00 {
		t.Fatalf("want BudgetExceeded with 150.00 available, got %v", err)
	}
	var ve *ValidationError
	if err := plan.CheckReservation(CategoryEquipment, 1, claims, bookings); !errors.As(err, &ve) {
		t.Fatalf("no allocation: %v", err)
	}
}

func TestBudgetsIncludeReservations(t *testing.T) {
	claims := []Claim{{ProviderID: "PR1", Category: CategoryDailyLiving, Amount: 300_00, Status: ClaimSubmitted}}
	bookings := []ServiceBooking{{ProviderID: "PR1", PlanID: "PL1", Category: CategoryDailyLiving, Reserved: 1_000_00}}
	got := activePlan().Budgets(claims, bookings)
	if got[0].Spent != 300_00 || got[0].Reserved != 700_00 || got[0].Remaining != 700_00 {
		t.Fatalf("unexpected budget %+v", got[0])
	}
}

// A reservation is a hold on top of what the provider has already consumed:
// claims beyond an earlier reservation do not eat into a later one.
func TestReserveTopsUpFromWhatTheProviderConsumed(t *testing.T) {
	plan := activePlan() // THERAPY 500.00
	claim := func(amount Money, status ClaimStatus) Claim {
		return Claim{ProviderID: "PR1", Category: CategoryTherapy, Amount: amount, Status: status}
	}
	booking := func(reserved Money) ServiceBooking {
		return ServiceBooking{ProviderID: "PR1", PlanID: "PL1", Category: CategoryTherapy, Reserved: reserved}
	}
	for _, tc := range []struct {
		name            string
		claims          []Claim
		bookings        []ServiceBooking
		amount          Money
		wantReserved    Money
		wantOutstanding Money
	}{
		{"first reservation", nil, nil, 80_00, 80_00, 80_00},
		{"unclaimed part of an earlier reservation stays held", []Claim{claim(40_00, ClaimApproved)}, []ServiceBooking{booking(100_00)}, 50_00, 150_00, 110_00},
		{"an over-claimed reservation is topped up from the claims", []Claim{claim(150_00, ClaimSubmitted)}, []ServiceBooking{booking(100_00)}, 80_00, 230_00, 80_00},
		{"rejected claims consume nothing", []Claim{claim(150_00, ClaimRejected)}, []ServiceBooking{booking(100_00)}, 80_00, 180_00, 180_00},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, err := plan.Reserve("PR1", CategoryTherapy, tc.amount, tc.claims, tc.bookings)
			if err != nil {
				t.Fatalf("reserve: %v", err)
			}
			if want := (ServiceBooking{ProviderID: "PR1", PlanID: "PL1", Category: CategoryTherapy, Reserved: tc.wantReserved}); b != want {
				t.Fatalf("booking = %+v, want %+v", b, want)
			}
			if got := OutstandingReserved(tc.claims, []ServiceBooking{b}, CategoryTherapy); got != tc.wantOutstanding {
				t.Fatalf("outstanding after the reservation = %v, want %v", got, tc.wantOutstanding)
			}
		})
	}
}

func TestReserveRefusesWhatDoesNotFit(t *testing.T) {
	plan := activePlan() // THERAPY 500.00
	claims := []Claim{{ProviderID: "PR2", Category: CategoryTherapy, Amount: 400_00, Status: ClaimApproved}}
	var be *BudgetExceededError
	if _, err := plan.Reserve("PR1", CategoryTherapy, 100_01, claims, nil); !errors.As(err, &be) || be.Remaining != 100_00 {
		t.Fatalf("want BudgetExceeded with 100.00 available, got %v", err)
	}
}

// When claims already exceed the allocation (an allocation lowered after the
// fact), the refusal reports nothing remaining, never a negative amount.
func TestCheckReservationNeverReportsANegativeRemaining(t *testing.T) {
	claims := []Claim{{ProviderID: "PR1", Category: CategoryTherapy, Amount: 600_00, Status: ClaimApproved}}
	var be *BudgetExceededError
	if err := activePlan().CheckReservation(CategoryTherapy, 1, claims, nil); !errors.As(err, &be) || be.Remaining != 0 {
		t.Fatalf("want BudgetExceeded with 0 remaining, got %v", err)
	}
}
