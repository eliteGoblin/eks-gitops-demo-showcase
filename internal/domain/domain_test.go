package domain

import (
	"errors"
	"testing"
	"time"
)

func TestParseMoney(t *testing.T) {
	valid := map[string]Money{"123.45": 12345, "7": 700, "0.5": 50, "-1.05": -105, " 3.10 ": 310}
	for in, want := range valid {
		got, err := ParseMoney(in)
		if err != nil || got != want {
			t.Errorf("ParseMoney(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"abc", "1.234", "", ".", "1.x", "--5", "+5", "1e3", "1_0", "-", "1.", ".5", "-.5", "1.-5", "0x10"} {
		if _, err := ParseMoney(bad); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("ParseMoney(%q) = %v, want ErrInvalidInput", bad, err)
		}
	}
}

func TestMoneyString(t *testing.T) {
	tests := []struct {
		in   Money
		want string
	}{{100000005, "1000000.05"}, {-5, "-0.05"}, {0, "0.00"}, {12345, "123.45"}}
	for _, tc := range tests {
		if got := tc.in.String(); got != tc.want {
			t.Errorf("Money(%d).String() = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestParseDate(t *testing.T) {
	if _, err := ParseDate("2026-02-30"); !errors.Is(err, ErrInvalidInput) {
		t.Error("invalid date accepted")
	}
	if d, err := ParseDate("1990-05-04"); err != nil || d != "1990-05-04" {
		t.Errorf("valid date rejected: %v", err)
	}
}

func activePlan() Plan {
	return Plan{ID: "PL1", Status: PlanActive, StartDate: "2026-01-01", EndDate: "2026-12-31", Allocations: []Allocation{
		{Category: CategoryDailyLiving, Allocated: 1000_00},
		{Category: CategoryTherapy, Allocated: 500_00},
	}}
}

func TestPlanCheckClaim(t *testing.T) {
	existing := []Claim{
		{Category: CategoryDailyLiving, Amount: 300_00, Status: ClaimApproved},
		{Category: CategoryDailyLiving, Amount: 100_00, Status: ClaimRejected}, // does not consume budget
		{Category: CategoryTherapy, Amount: 50_00, Status: ClaimSubmitted},
	}
	inactive := activePlan()
	inactive.Status = PlanExpired

	tests := []struct {
		name      string
		plan      Plan
		category  Category
		amount    Money
		date      Date
		wantField string // ValidationError field, "" if none
		wantOver  *BudgetExceededError
	}{
		{"fits", activePlan(), CategoryDailyLiving, 700_00, "2026-05-01", "", nil},
		{"over budget", activePlan(), CategoryDailyLiving, 700_01, "2026-05-01", "", &BudgetExceededError{Remaining: 700_00, Requested: 700_01}},
		{"no allocation", activePlan(), CategoryEquipment, 1, "2026-05-01", "category", nil},
		{"inactive plan", inactive, CategoryDailyLiving, 1, "2026-05-01", "planId", nil},
		{"first day of plan", activePlan(), CategoryDailyLiving, 1, "2026-01-01", "", nil},
		{"last day of plan", activePlan(), CategoryDailyLiving, 1, "2026-12-31", "", nil},
		{"before plan", activePlan(), CategoryDailyLiving, 1, "2025-12-31", "serviceDate", nil},
		{"after plan", activePlan(), CategoryDailyLiving, 1, "2027-01-01", "serviceDate", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.plan.CheckClaim("PR-A", tc.category, tc.amount, tc.date, existing, nil)
			var ve *ValidationError
			var be *BudgetExceededError
			switch {
			case tc.wantField != "":
				if !errors.As(err, &ve) || ve.Field != tc.wantField {
					t.Fatalf("want ValidationError on %q, got %v", tc.wantField, err)
				}
			case tc.wantOver != nil:
				if !errors.As(err, &be) || *be != *tc.wantOver {
					t.Fatalf("want %+v, got %v", tc.wantOver, err)
				}
			default:
				if err != nil {
					t.Fatalf("want nil, got %v", err)
				}
			}
		})
	}
}

func TestPlanBudgets(t *testing.T) {
	claims := []Claim{{Category: CategoryTherapy, Amount: 120_50, Status: ClaimSubmitted}}
	got := activePlan().Budgets(claims, nil)
	if len(got) != 2 || got[1].Spent != 120_50 || got[1].Remaining != 379_50 || got[0].Spent != 0 {
		t.Fatalf("unexpected budgets: %+v", got)
	}
}

func TestNewClaimAndApprove(t *testing.T) {
	now := time.Date(2026, 5, 1, 9, 0, 0, 0, time.FixedZone("AEST", 10*3600))
	if _, err := NewClaim("C1", "PL1", "PR1", CategoryTherapy, 0, "2026-05-01", now); err == nil {
		t.Fatal("zero amount accepted")
	}
	c, err := NewClaim("C1", "PL1", "PR1", CategoryTherapy, 10_00, "2026-05-01", now)
	if err != nil || c.Status != ClaimSubmitted || !c.SubmittedAt.Equal(now) || c.SubmittedAt.Location() != time.UTC {
		t.Fatalf("NewClaim = %+v, %v", c, err)
	}
	if err := c.Approve(); err != nil || c.Status != ClaimApproved {
		t.Fatalf("approve: %v status %s", err, c.Status)
	}
	var ve *ValidationError
	if err := c.Approve(); !errors.As(err, &ve) {
		t.Fatalf("second approve must fail with ValidationError, got %v", err)
	}
}

func TestErrorMessages(t *testing.T) {
	if (&ValidationError{Field: "f", Message: "m"}).Error() != "f: m" {
		t.Error("ValidationError message")
	}
	if (&BudgetExceededError{Remaining: 100, Requested: 200}).Error() != "budget exceeded: requested 2.00, remaining 1.00" {
		t.Error("BudgetExceededError message")
	}
}

// A provider may claim what is neither spent nor reserved for another
// provider; its own outstanding reservation stays available to it.
func TestClaimCannotTakeAnotherProvidersReservation(t *testing.T) {
	plan := activePlan() // DAILY_LIVING allocated 1000.00
	claims := []Claim{{ProviderID: "PR-A", Category: CategoryDailyLiving, Amount: 300_00, Status: ClaimApproved}}
	bookings := []ServiceBooking{
		{ProviderID: "PR-A", PlanID: "PL1", Category: CategoryDailyLiving, Reserved: 300_00}, // fully consumed by the claim
		{ProviderID: "PR-B", PlanID: "PL1", Category: CategoryDailyLiving, Reserved: 600_00}, // untouched
	}
	tests := []struct {
		name      string
		provider  string
		amount    Money
		remaining Money // > 0 means BudgetExceededError with that remaining
	}{
		{"A takes the unreserved rest", "PR-A", 100_00, 0},
		{"A cannot take B's reservation", "PR-A", 100_01, 100_00},
		{"B uses its own reservation plus the rest", "PR-B", 700_00, 0},
		{"B cannot exceed it", "PR-B", 700_01, 700_00},
		{"an unbooked provider gets only the unreserved rest", "PR-C", 100_01, 100_00},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := plan.CheckClaim(tc.provider, CategoryDailyLiving, tc.amount, "2026-05-01", claims, bookings)
			var be *BudgetExceededError
			switch {
			case tc.remaining == 0 && err != nil:
				t.Fatalf("want the claim accepted, got %v", err)
			case tc.remaining > 0 && (!errors.As(err, &be) || be.Remaining != tc.remaining || be.Requested != tc.amount):
				t.Fatalf("want BudgetExceededError{Remaining:%v}, got %v", tc.remaining, err)
			}
		})
	}
}
