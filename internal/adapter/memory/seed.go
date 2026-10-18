package memory

import (
	"time"

	"github.com/eliteGoblin/eks-gitops-demo/internal/domain"
)

func dob(s string) *domain.Date { d := domain.Date(s); return &d }

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err) // seed literals are fixed
	}
	return t
}

// Seed loads the demo data set; internal/adapter/postgres/migrations seeds the same. Identifiers match the Keycloak user attributes:
// zhaolin.sun -> participant P1, demo.guest -> participant P3,
// sam.nominee -> nominee N1 (acts for P1), riley.provider -> provider PR1.
func Seed(s *Store) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.participants["P1"] = domain.Participant{ID: "P1", SchemeNumber: "SN100001", GivenName: "Zhaolin", FamilyName: "Sun", DateOfBirth: dob("1990-05-04")}
	s.participants["P2"] = domain.Participant{ID: "P2", SchemeNumber: "SN100002", GivenName: "Peter", FamilyName: "Quinn", DateOfBirth: dob("1985-11-20")}
	s.participants["P3"] = domain.Participant{ID: "P3", SchemeNumber: "SN100003", GivenName: "Jordan", FamilyName: "Lee", DateOfBirth: dob("1996-08-12")}

	s.plans["PL1"] = domain.Plan{ID: "PL1", ParticipantID: "P1", Status: domain.PlanActive, StartDate: "2026-01-01", EndDate: "2026-12-31",
		Allocations: []domain.Allocation{{Category: domain.CategoryDailyLiving, Allocated: 10_000_00}, {Category: domain.CategoryTherapy, Allocated: 5_000_00}}}
	s.plans["PL2"] = domain.Plan{ID: "PL2", ParticipantID: "P2", Status: domain.PlanActive, StartDate: "2026-03-01", EndDate: "2027-02-28",
		Allocations: []domain.Allocation{{Category: domain.CategoryDailyLiving, Allocated: 8_000_00}, {Category: domain.CategoryEquipment, Allocated: 2_500_00}}}
	s.plans["PL3"] = domain.Plan{ID: "PL3", ParticipantID: "P3", Status: domain.PlanActive, StartDate: "2026-06-01", EndDate: "2027-05-31",
		Allocations: []domain.Allocation{{Category: domain.CategoryDailyLiving, Allocated: 6_000_00}, {Category: domain.CategoryTherapy, Allocated: 3_000_00}}}

	s.providers["PR1"] = domain.Provider{ID: "PR1", Name: "Sunrise Care", ABN: "12345678901"}
	s.providers["PR2"] = domain.Provider{ID: "PR2", Name: "Harbour Therapy", ABN: "98765432109"}

	s.bookings = []domain.ServiceBooking{{ProviderID: "PR1", PlanID: "PL1", Category: domain.CategoryDailyLiving, Reserved: 1_000_00}}
	s.delegations = []domain.Delegation{{NomineeID: "N1", ParticipantID: "P1", Active: true}}

	s.claims["C1"] = domain.Claim{ID: "C1", PlanID: "PL1", ProviderID: "PR1", Category: domain.CategoryDailyLiving, Amount: 250_00, Status: domain.ClaimApproved, ServiceDate: "2026-02-10", SubmittedAt: at("2026-02-11T09:00:00Z")}
	s.claims["C2"] = domain.Claim{ID: "C2", PlanID: "PL1", ProviderID: "PR1", Category: domain.CategoryDailyLiving, Amount: 180_50, Status: domain.ClaimSubmitted, ServiceDate: "2026-03-03", SubmittedAt: at("2026-03-04T10:30:00Z")}
	s.claims["C3"] = domain.Claim{ID: "C3", PlanID: "PL2", ProviderID: "PR2", Category: domain.CategoryDailyLiving, Amount: 400_00, Status: domain.ClaimSubmitted, ServiceDate: "2026-04-01", SubmittedAt: at("2026-04-02T08:00:00Z")}
}
