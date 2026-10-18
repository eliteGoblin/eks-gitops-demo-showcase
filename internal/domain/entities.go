package domain

// Category is a support category a plan budget is split into.
type Category string

const (
	CategoryDailyLiving Category = "DAILY_LIVING"
	CategoryTherapy     Category = "THERAPY"
	CategoryEquipment   Category = "EQUIPMENT"
)

type PlanStatus string

const (
	PlanDraft   PlanStatus = "DRAFT"
	PlanActive  PlanStatus = "ACTIVE"
	PlanExpired PlanStatus = "EXPIRED"
)

type ClaimStatus string

const (
	ClaimSubmitted ClaimStatus = "SUBMITTED"
	ClaimApproved  ClaimStatus = "APPROVED"
	ClaimRejected  ClaimStatus = "REJECTED"
	ClaimPaid      ClaimStatus = "PAID"
)

// Participant is a person who holds plans. DateOfBirth is nil when the caller
// may not see it (field masking happens in the app layer).
type Participant struct {
	ID           string
	SchemeNumber string
	GivenName    string
	FamilyName   string
	DateOfBirth  *Date
}

// Allocation is the amount allocated to one support category of a plan.
type Allocation struct {
	Category  Category
	Allocated Money
}

type Plan struct {
	ID            string
	ParticipantID string
	Status        PlanStatus
	StartDate     Date
	EndDate       Date
	Allocations   []Allocation
}

// Budget is a view over one allocation: Spent is derived from claims,
// Reserved is what service bookings hold but have not claimed yet, and
// Remaining is what has not been spent (reservations are not deducted).
type Budget struct {
	Category  Category
	Allocated Money
	Spent     Money
	Reserved  Money
	Remaining Money
}

type Provider struct {
	ID   string
	Name string
	ABN  string
}

// Delegation lets a nominee act for a participant while it is active.
type Delegation struct {
	NomineeID     string
	ParticipantID string
	Active        bool
}

// ServiceBooking links a provider to one support category of one plan and
// reserves part of that category's budget for the provider.
type ServiceBooking struct {
	ProviderID string
	PlanID     string
	Category   Category
	Reserved   Money
}
