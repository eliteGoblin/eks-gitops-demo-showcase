package graphql_test

import (
	"context"
	"net/http"
	"reflect"
	"sync"
	"testing"

	"github.com/eliteGoblin/eks-gitops-demo/internal/adapter/memory"
	"github.com/eliteGoblin/eks-gitops-demo/internal/app"
	"github.com/eliteGoblin/eks-gitops-demo/internal/domain"
)

// recordingAudit keeps every audit event; take hands them over and starts afresh.
type recordingAudit struct {
	mu     sync.Mutex
	events []app.AuditEvent
}

func (r *recordingAudit) Record(_ context.Context, e app.AuditEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *recordingAudit) take() []app.AuditEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.events
	r.events = nil
	return out
}

// auditedServer is server(t) with every audit event recorded.
func auditedServer(t *testing.T) (http.Handler, *recordingAudit) {
	t.Helper()
	st := memory.New()
	memory.Seed(st)
	audit := &recordingAudit{}
	return serverAudited(t, st, nil, audit, nil), audit
}

// A field the role gate refuses never reaches its use case, so the gate
// audits the attempt: exactly one deny, naming the use case's action, the
// record from the arguments and the role the caller lacked.
func TestRoleGateRefusalsAreAudited(t *testing.T) {
	h, audit := auditedServer(t)
	deny := func(pr *app.Principal, a app.Action, res app.Resource, reason string) app.AuditEvent {
		return app.AuditEvent{Actor: pr.Subject, Username: pr.Username, Roles: pr.RoleNames(), Action: a, Resource: res, Outcome: "deny", Detail: reason}
	}
	submit := `mutation { submitClaim(input: {planId: "PL1", category: DAILY_LIVING, amount: "1.00", serviceDate: "2026-05-01"}) { __typename } }`
	approve := `mutation { approveClaim(id: "C2", reason: "x") { __typename } }`
	tests := []struct {
		name  string
		pr    *app.Principal
		query string
		want  app.AuditEvent
	}{
		{"participant approves", participant, approve, deny(participant, app.ActionApproveClaim, app.Resource{ClaimID: "C2"}, "requires role agency_staff")},
		{"provider approves", provider, approve, deny(provider, app.ActionApproveClaim, app.Resource{ClaimID: "C2"}, "requires role agency_staff")},
		{"participant submits", participant, submit,
			deny(participant, app.ActionSubmitClaim, app.Resource{PlanID: "PL1", Category: domain.CategoryDailyLiving}, "requires role provider")},
		{"staff submits", staff, submit,
			deny(staff, app.ActionSubmitClaim, app.Resource{PlanID: "PL1", Category: domain.CategoryDailyLiving}, "requires role provider")},
		{"provider raises", provider, `mutation { raiseSupportRequest(input: {planId: "PL1", category: THERAPY, description: "x", estimatedAmount: "1.00"}) { __typename } }`,
			deny(provider, app.ActionRaiseSupportRequest, app.Resource{PlanID: "PL1", Category: domain.CategoryTherapy}, "requires role participant or nominee or agency_staff")},
		{"provider decides", provider, `mutation { decideSupportRequest(input: {id: "SR9", decision: APPROVE, reason: "x"}) { __typename } }`,
			deny(provider, app.ActionDecideSupportRequest, app.Resource{SupportRequestID: "SR9"}, "requires role agency_staff")},
		{"participant reads the queue", participant, `{ pendingSupportRequests { id } }`,
			deny(participant, app.ActionReadSupportQueue, app.Resource{}, "requires role agency_staff")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if r := do(t, h, tc.pr, tc.query); code(r) != "FORBIDDEN" {
				t.Fatalf("want FORBIDDEN, got %+v", r.Errors)
			}
			if got := audit.take(); !reflect.DeepEqual(got, []app.AuditEvent{tc.want}) {
				t.Fatalf("audit = %+v\nwant exactly [%+v]", got, tc.want)
			}
		})
	}
}

// An approval the gate lets through is audited by its use case alone: the
// decision names the claim from the arguments (no gate event on top), and
// the outcome names it with its plan and parties.
func TestAllowedApprovalIsAuditedOnceWithTheClaim(t *testing.T) {
	h, audit := auditedServer(t)
	if r := do(t, h, staff, `mutation { approveClaim(id: "C2", reason: "ok") { __typename } }`); len(r.Errors) > 0 {
		t.Fatalf("approve: %+v", r.Errors)
	}
	by := app.AuditEvent{Actor: staff.Subject, Roles: staff.RoleNames(), Action: app.ActionApproveClaim}
	allow, approved := by, by
	allow.Resource, allow.Outcome = app.Resource{ClaimID: "C2"}, "allow"
	approved.Resource = app.Resource{ParticipantID: "P1", PlanID: "PL1", ProviderID: "PR1", Category: domain.CategoryDailyLiving, ClaimID: "C2"}
	approved.Outcome, approved.Detail = "approved", "ok"
	if got, want := audit.take(), []app.AuditEvent{allow, approved}; !reflect.DeepEqual(got, want) {
		t.Fatalf("audit = %+v\nwant %+v", got, want)
	}
}
