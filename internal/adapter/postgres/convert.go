package postgres

import (
	"time"

	"github.com/eliteGoblin/eks-gitops-demo/internal/adapter/postgres/sqlcdb"
	"github.com/eliteGoblin/eks-gitops-demo/internal/domain"
)

// Rows and domain values differ only in types; these map between them.

const dateLayout = "2006-01-02"

func mapRows[R, D any](rows []R, f func(R) D) []D {
	out := make([]D, len(rows))
	for i, r := range rows {
		out[i] = f(r)
	}
	return out
}

func toDate(t time.Time) domain.Date { return domain.Date(t.Format(dateLayout)) }

// fromDate parses a domain date; domain dates are validated on the way in.
func fromDate(d domain.Date) time.Time {
	t, _ := time.Parse(dateLayout, string(d))
	return t
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func value(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func nonNil(ids []string) []string {
	if ids == nil {
		return []string{}
	}
	return ids
}

func toParticipant(p sqlcdb.Participant) domain.Participant {
	out := domain.Participant{ID: p.ID, SchemeNumber: p.SchemeNumber, GivenName: p.GivenName, FamilyName: p.FamilyName}
	if p.DateOfBirth != nil {
		dob := toDate(*p.DateOfBirth)
		out.DateOfBirth = &dob
	}
	return out
}

func toProvider(p sqlcdb.Provider) domain.Provider {
	return domain.Provider{ID: p.ID, Name: p.Name, ABN: p.Abn}
}

func toPlan(p sqlcdb.Plan, allocs []domain.Allocation) domain.Plan {
	if allocs == nil {
		allocs = []domain.Allocation{}
	}
	return domain.Plan{ID: p.ID, ParticipantID: p.ParticipantID, Status: domain.PlanStatus(p.Status),
		StartDate: toDate(p.StartDate), EndDate: toDate(p.EndDate), Allocations: allocs}
}

func toClaim(c sqlcdb.Claim) domain.Claim {
	out := domain.Claim{ID: c.ID, PlanID: c.PlanID, ProviderID: c.ProviderID, Category: domain.Category(c.Category),
		Amount: domain.Money(c.AmountCents), Status: domain.ClaimStatus(c.Status), ServiceDate: toDate(c.ServiceDate),
		SubmittedAt: c.SubmittedAt.UTC()}
	if c.PaymentStatus != nil {
		out.Payment = &domain.ClaimPayment{Status: domain.PaymentStatus(*c.PaymentStatus), Ref: value(c.PaymentRef)}
	}
	return out
}

func updateClaimParams(c domain.Claim, from domain.ClaimStatus) sqlcdb.UpdateClaimParams {
	p := sqlcdb.UpdateClaimParams{ID: c.ID, Status: string(c.Status), FromStatus: string(from)}
	if c.Payment != nil {
		status := string(c.Payment.Status)
		p.PaymentStatus, p.PaymentRef = &status, optional(c.Payment.Ref)
	}
	return p
}

func insertClaimParams(c domain.Claim) sqlcdb.InsertClaimParams {
	return sqlcdb.InsertClaimParams{ID: c.ID, PlanID: c.PlanID, ProviderID: c.ProviderID, Category: string(c.Category),
		AmountCents: int64(c.Amount), Status: string(c.Status), ServiceDate: fromDate(c.ServiceDate), SubmittedAt: c.SubmittedAt}
}

func toBooking(b sqlcdb.ServiceBooking) domain.ServiceBooking {
	return domain.ServiceBooking{ProviderID: b.ProviderID, PlanID: b.PlanID, Category: domain.Category(b.Category), Reserved: domain.Money(b.ReservedCents)}
}

func toSupportRequest(r sqlcdb.SupportRequest) domain.SupportRequest {
	out := domain.SupportRequest{
		ID: r.ID, PlanID: r.PlanID, ParticipantID: r.ParticipantID, RequestedBy: r.RequestedBy, RequestedByName: r.RequestedByName,
		Category: domain.Category(r.Category), Description: r.Description, EstimatedAmount: domain.Money(r.EstimatedCents),
		PreferredProviderID: value(r.PreferredProviderID), Status: domain.SupportRequestStatus(r.Status),
		DecidedBy: value(r.DecidedBy), DecisionReason: value(r.DecisionReason), BookedProviderID: value(r.BookedProviderID),
		RaisedAt: r.RaisedAt.UTC(),
	}
	if r.DecidedAt != nil {
		at := r.DecidedAt.UTC()
		out.DecidedAt = &at
	}
	return out
}
