package app

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/eliteGoblin/eks-gitops-demo/internal/domain"
)

// fakeRel mirrors the seed: nominee N1 -> P1; provider PR1 booked on PL1/DAILY_LIVING; PL1 belongs to P1.
type fakeRel struct{ err error }

func (f fakeRel) IsActiveDelegate(_ context.Context, nomineeID, participantID string) (bool, error) {
	return nomineeID == "N1" && participantID == "P1", f.err
}

func (f fakeRel) DelegatedParticipants(_ context.Context, nomineeID string) ([]string, error) {
	if nomineeID == "N1" {
		return []string{"P1"}, f.err
	}
	return nil, f.err
}

func (f fakeRel) BookedCategories(_ context.Context, providerID, planID string) ([]domain.Category, error) {
	if providerID == "PR1" && planID == "PL1" {
		return []domain.Category{domain.CategoryDailyLiving}, f.err
	}
	return nil, f.err
}

func (f fakeRel) HasBookingForParticipant(_ context.Context, providerID, participantID string) (bool, error) {
	return providerID == "PR1" && participantID == "P1", f.err
}

func (f fakeRel) BookedParticipants(_ context.Context, providerID string) ([]string, error) {
	if providerID == "PR1" {
		return []string{"P1"}, f.err
	}
	return nil, f.err
}

func principal(role Role, id string) Principal {
	p := Principal{Subject: "sub-" + string(role), Roles: map[Role]bool{role: true}}
	switch role {
	case RoleParticipant:
		p.ParticipantID = id
	case RoleNominee:
		p.NomineeID = id
	case RoleProvider:
		p.ProviderID = id
	}
	return p
}

func TestPolicyMatrix(t *testing.T) {
	participant := principal(RoleParticipant, "P1")
	nominee := principal(RoleNominee, "N1")
	provider := principal(RoleProvider, "PR1")
	staff := principal(RoleAgencyStaff, "")
	staffProvider := principal(RoleProvider, "PR1")
	staffProvider.Roles[RoleAgencyStaff] = true
	p1 := Resource{ParticipantID: "P1"}
	p2 := Resource{ParticipantID: "P2"}
	pl1 := Resource{ParticipantID: "P1", PlanID: "PL1"}
	pl2 := Resource{ParticipantID: "P2", PlanID: "PL2"}
	claimPR1 := Resource{ParticipantID: "P1", PlanID: "PL1", ProviderID: "PR1", Category: domain.CategoryDailyLiving}
	claimPR2 := Resource{ParticipantID: "P2", PlanID: "PL2", ProviderID: "PR2", Category: domain.CategoryDailyLiving}

	tests := []struct {
		name  string
		pr    Principal
		a     Action
		res   Resource
		allow bool
	}{
		{"staff reads any participant", staff, ActionReadParticipant, p2, true},
		{"staff approves", staff, ActionApproveClaim, claimPR2, true},
		{"staff reads any plan", staff, ActionReadPlan, pl2, true},
		{"staff reads any claim", staff, ActionReadClaim, claimPR2, true},
		{"staff cannot submit claims", staff, ActionSubmitClaim, claimPR1, false},
		{"staff+provider submits only on booked category", staffProvider, ActionSubmitClaim, claimPR1, true},
		{"staff+provider cannot submit on unbooked plan", staffProvider, ActionSubmitClaim, Resource{ParticipantID: "P2", PlanID: "PL2", Category: domain.CategoryDailyLiving}, false},
		{"unknown action denied for staff", staff, Action("plan:delete"), pl1, false},
		{"unknown action denied for participant", participant, Action("plan:delete"), pl1, false},

		{"participant reads own", participant, ActionReadParticipant, p1, true},
		{"participant cannot read other", participant, ActionReadParticipant, p2, false},
		{"participant reads own plan", participant, ActionReadPlan, pl1, true},
		{"participant reads claims on own plan", participant, ActionReadClaim, claimPR1, true},
		{"participant cannot submit", participant, ActionSubmitClaim, claimPR1, false},
		{"participant cannot approve", participant, ActionApproveClaim, claimPR1, false},

		{"nominee reads delegated participant", nominee, ActionReadParticipant, p1, true},
		{"nominee cannot read undelegated", nominee, ActionReadParticipant, p2, false},
		{"nominee reads delegated plan", nominee, ActionReadPlan, pl1, true},
		{"nominee cannot read other plan", nominee, ActionReadPlan, pl2, false},
		{"nominee cannot submit", nominee, ActionSubmitClaim, claimPR1, false},

		{"provider reads booked participant", provider, ActionReadParticipant, p1, true},
		{"provider cannot read unbooked participant", provider, ActionReadParticipant, p2, false},
		{"provider reads booked plan", provider, ActionReadPlan, pl1, true},
		{"provider cannot read unbooked plan", provider, ActionReadPlan, pl2, false},
		{"provider reads own claim", provider, ActionReadClaim, claimPR1, true},
		{"provider cannot read other provider claim", provider, ActionReadClaim, claimPR2, false},
		{"provider submits on booked category", provider, ActionSubmitClaim, claimPR1, true},
		{"provider cannot submit on unbooked category", provider, ActionSubmitClaim, Resource{ParticipantID: "P1", PlanID: "PL1", Category: domain.CategoryTherapy}, false},
		{"provider cannot submit on unbooked plan", provider, ActionSubmitClaim, Resource{ParticipantID: "P2", PlanID: "PL2", Category: domain.CategoryDailyLiving}, false},
		{"provider cannot approve", provider, ActionApproveClaim, claimPR1, false},

		{"participant raises for own plan", participant, ActionRaiseSupportRequest, pl1, true},
		{"participant cannot raise for other plan", participant, ActionRaiseSupportRequest, pl2, false},
		{"nominee raises for delegated plan", nominee, ActionRaiseSupportRequest, pl1, true},
		{"nominee cannot raise for other plan", nominee, ActionRaiseSupportRequest, pl2, false},
		{"staff raises for anyone", staff, ActionRaiseSupportRequest, pl2, true},
		{"provider cannot raise", provider, ActionRaiseSupportRequest, pl1, false},
		{"staff decides", staff, ActionDecideSupportRequest, pl2, true},
		{"participant cannot decide", participant, ActionDecideSupportRequest, pl1, false},
		{"staff reads support queue", staff, ActionReadSupportQueue, Resource{}, true},
		{"participant cannot read support queue", participant, ActionReadSupportQueue, Resource{}, false},

		{"no roles denied", Principal{Subject: "x", Roles: map[Role]bool{}}, ActionReadParticipant, p1, false},
		{"provider without provider id denied", Principal{Subject: "x", Roles: map[Role]bool{RoleProvider: true}}, ActionSubmitClaim, claimPR1, false},
	}
	policy := NewPolicy(fakeRel{})
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := policy.Can(context.Background(), tc.pr, tc.a, tc.res)
			if tc.allow && err != nil {
				t.Fatalf("expected allow, got %v", err)
			}
			if !tc.allow && !errors.Is(err, ErrForbidden) {
				t.Fatalf("expected ErrForbidden, got %v", err)
			}
		})
	}
}

func TestPolicyPropagatesLookupErrors(t *testing.T) {
	boom := errors.New("db down")
	policy := NewPolicy(fakeRel{err: boom})
	ctx := context.Background()
	if err := policy.Can(ctx, principal(RoleNominee, "N1"), ActionReadParticipant, Resource{ParticipantID: "P1"}); !errors.Is(err, boom) {
		t.Fatalf("nominee lookup error not propagated: %v", err)
	}
	if err := policy.Can(ctx, principal(RoleProvider, "PR1"), ActionSubmitClaim, Resource{PlanID: "PL1"}); !errors.Is(err, boom) {
		t.Fatalf("booking lookup error not propagated: %v", err)
	}
	if err := policy.Can(ctx, principal(RoleProvider, "PR1"), ActionReadParticipant, Resource{ParticipantID: "P1"}); !errors.Is(err, boom) {
		t.Fatalf("provider read lookup error not propagated: %v", err)
	}
}

func TestFieldMasking(t *testing.T) {
	policy := NewPolicy(fakeRel{})
	ctx := context.Background()
	cases := []struct {
		name string
		pr   Principal
		see  bool
	}{
		{"participant sees own DOB", principal(RoleParticipant, "P1"), true},
		{"nominee sees delegated DOB", principal(RoleNominee, "N1"), true},
		{"provider never sees DOB", principal(RoleProvider, "PR1"), false},
		{"staff sees DOB", principal(RoleAgencyStaff, ""), true},
	}
	for _, tc := range cases {
		got, err := policy.CanSeeDateOfBirth(ctx, tc.pr, "P1")
		if err != nil || got != tc.see {
			t.Errorf("%s: got %v err %v, want %v", tc.name, got, err, tc.see)
		}
	}
	daily := Resource{ParticipantID: "P1", PlanID: "PL1", Category: domain.CategoryDailyLiving}
	therapy := Resource{ParticipantID: "P1", PlanID: "PL1", Category: domain.CategoryTherapy}
	budgetCases := []struct {
		name string
		pr   Principal
		res  Resource
		see  bool
	}{
		{"provider sees booked category", principal(RoleProvider, "PR1"), daily, true},
		{"provider does not see unbooked category", principal(RoleProvider, "PR1"), therapy, false},
		{"participant sees all own budgets", principal(RoleParticipant, "P1"), therapy, true},
		{"staff sees budgets", principal(RoleAgencyStaff, ""), therapy, true},
		{"stranger sees nothing", Principal{Roles: map[Role]bool{}}, daily, false},
	}
	for _, tc := range budgetCases {
		if got, err := policy.CanSeeBudget(ctx, tc.pr, tc.res); err != nil || got != tc.see {
			t.Errorf("%s: got %v err %v, want %v", tc.name, got, err, tc.see)
		}
	}
}

func TestPrincipalRoles(t *testing.T) {
	p := Principal{Roles: map[Role]bool{RoleAgencyStaff: true, RoleParticipant: true}}
	got := p.RoleNames()
	if len(got) != 2 || got[0] != RoleParticipant || got[1] != RoleAgencyStaff {
		t.Fatalf("RoleNames = %v", got)
	}
	if _, ok := PrincipalFrom(context.Background()); ok {
		t.Fatal("empty context must not carry a principal")
	}
	if got, ok := PrincipalFrom(WithPrincipal(context.Background(), p)); !ok || !got.HasRole(RoleAgencyStaff) {
		t.Fatal("principal round trip failed")
	}
}

type recordedEvents struct{ events []AuditEvent }

func (r *recordedEvents) Record(_ context.Context, e AuditEvent) { r.events = append(r.events, e) }

func TestGuardAuditsOutcome(t *testing.T) {
	tests := []struct {
		name    string
		rel     fakeRel
		pr      Principal
		outcome string
	}{
		{"allowed", fakeRel{}, principal(RoleParticipant, "P1"), "allow"},
		{"denied", fakeRel{}, principal(RoleParticipant, "P2"), "deny"},
		{"lookup failure is an error, not a denial", fakeRel{err: errors.New("db down")}, principal(RoleNominee, "N1"), "error"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			audit := &recordedEvents{}
			g := guard{policy: NewPolicy(tc.rel), audit: audit}
			_, _ = g.authorize(WithPrincipal(context.Background(), tc.pr), ActionReadParticipant, Resource{ParticipantID: "P1"})
			if len(audit.events) != 1 || audit.events[0].Outcome != tc.outcome {
				t.Fatalf("audit = %+v, want outcome %q", audit.events, tc.outcome)
			}
		})
	}
}

func TestScopes(t *testing.T) {
	staffProvider := principal(RoleProvider, "PR1")
	staffProvider.Roles[RoleAgencyStaff] = true
	both := principal(RoleParticipant, "P3")
	both.Roles[RoleNominee] = true
	both.NomineeID = "N1"
	tests := []struct {
		name         string
		pr           Principal
		claims, mine Scope
	}{
		{"staff", principal(RoleAgencyStaff, ""), Scope{All: true}, Scope{RequestedBy: "sub-agency_staff"}},
		{"participant", principal(RoleParticipant, "P1"), Scope{ParticipantIDs: []string{"P1"}}, Scope{ParticipantIDs: []string{"P1"}, RequestedBy: "sub-participant"}},
		{"nominee", principal(RoleNominee, "N1"), Scope{ParticipantIDs: []string{"P1"}}, Scope{ParticipantIDs: []string{"P1"}, RequestedBy: "sub-nominee"}},
		{"participant and nominee", both, Scope{ParticipantIDs: []string{"P3", "P1"}}, Scope{ParticipantIDs: []string{"P3", "P1"}, RequestedBy: "sub-participant"}},
		{"provider", principal(RoleProvider, "PR1"), Scope{ProviderID: "PR1"}, Scope{RequestedBy: "sub-provider"}},
		{"staff + provider reads all claims", staffProvider, Scope{All: true}, Scope{RequestedBy: "sub-provider"}},
		{"no roles", Principal{Subject: "x"}, Scope{}, Scope{RequestedBy: "x"}},
	}
	p := NewPolicy(fakeRel{})
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := p.ClaimScope(context.Background(), tc.pr)
			if err != nil || !reflect.DeepEqual(got, tc.claims) {
				t.Fatalf("ClaimScope = %+v, %v; want %+v", got, err, tc.claims)
			}
			mine, err := p.SupportRequestScope(context.Background(), tc.pr)
			if err != nil || !reflect.DeepEqual(mine, tc.mine) {
				t.Fatalf("SupportRequestScope = %+v, %v; want %+v", mine, err, tc.mine)
			}
		})
	}
	if (Scope{}).Empty() != true || (Scope{ProviderID: "x"}).Empty() {
		t.Fatal("Scope.Empty")
	}
	if _, err := NewPolicy(fakeRel{err: errors.New("db down")}).ClaimScope(context.Background(), principal(RoleNominee, "N1")); err == nil {
		t.Fatal("lookup errors must surface")
	}
}
