package app_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	"github.com/eliteGoblin/eks-gitops-demo/internal/app"
	"github.com/eliteGoblin/eks-gitops-demo/internal/domain"
)

// eventsFor returns the audit events that name the claim or support request id.
func (r *recordingAuditor) eventsFor(id string) []app.AuditEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []app.AuditEvent
	for _, e := range r.events {
		if e.Resource.ClaimID == id || e.Resource.SupportRequestID == id {
			out = append(out, e)
		}
	}
	return out
}

func outcomes(events []app.AuditEvent) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, string(e.Action)+":"+e.Outcome)
	}
	return out
}

// Every audit line about a claim names it: the trail must answer "who did
// what to which claim" without going through traces. That includes the
// approval's decision, allow or deny, made before the claim is read.
func TestClaimAuditEventsNameTheClaim(t *testing.T) {
	forEachStore(t, func(t *testing.T, env0 testEnv) {
		env := newEnvWith(t, env0.store, &fakeGateway{}, nil)
		c, err := env.svc.Claims.Submit(as(app.RoleProvider, "PR1"), app.SubmitClaimInput{
			PlanID: "PL1", Category: domain.CategoryDailyLiving, Amount: 100_00, ServiceDate: "2026-05-01"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := env.svc.Claims.Approve(as(app.RoleProvider, "PR1"), c.ID, "own claim"); !errors.Is(err, app.ErrForbidden) {
			t.Fatalf("a provider must not approve: %v", err)
		}
		staff := as(app.RoleAgencyStaff, "")
		if _, err := env.svc.Claims.Approve(staff, c.ID, "verified invoice"); err != nil {
			t.Fatal(err)
		}
		if got, err := env.svc.Claims.Get(staff, c.ID); err != nil || got == nil {
			t.Fatalf("read: %v %v", got, err)
		}
		got := outcomes(env.audit.eventsFor(c.ID))
		for _, want := range []string{"claim:submit:submitted", "claim:approve:deny", "claim:approve:allow", "claim:approve:approved", "claim:approve:paid", "claim:read:allow"} {
			if !slices.Contains(got, want) {
				t.Errorf("no %q audit event naming claim %s; got %v", want, c.ID, got)
			}
		}
	})
}

func TestSupportRequestAuditEventsNameTheRequest(t *testing.T) {
	forEachStore(t, func(t *testing.T, env testEnv) {
		r := raiseTherapyWithProvider(t, env.svc, as(app.RoleParticipant, "P1"), "PL1", 120_00, "PR1")
		decision := app.DecideSupportRequestInput{ID: r.ID, Approve: true, ProviderID: "PR1", Reason: "fits the plan"}
		if _, err := env.svc.SupportRequests.Decide(as(app.RoleParticipant, "P1"), decision); !errors.Is(err, app.ErrForbidden) {
			t.Fatalf("a participant must not decide: %v", err)
		}
		if _, err := env.svc.SupportRequests.Decide(as(app.RoleAgencyStaff, ""), decision); err != nil {
			t.Fatal(err)
		}
		got := outcomes(env.audit.eventsFor(r.ID))
		for _, want := range []string{"support_request:raise:raised", "support_request:decide:deny", "support_request:decide:allow", "support_request:decide:approved"} {
			if !slices.Contains(got, want) {
				t.Errorf("no %q audit event naming support request %s; got %v", want, r.ID, got)
			}
		}
	})
}

// One claim's whole trail, event by event, from submission to payment. The
// approval's decision names the claim (and nothing it has not read yet); the
// outcome events keep the claim's full record and their detail. Submit's
// decision has no claim id: the claim is created only once it is allowed.
func TestClaimTrailFromSubmissionToPayment(t *testing.T) {
	forEachStore(t, func(t *testing.T, env0 testEnv) {
		env := newEnvWith(t, env0.store, &fakeGateway{}, nil)
		c, err := env.svc.Claims.Submit(as(app.RoleProvider, "PR1"), app.SubmitClaimInput{
			PlanID: "PL1", Category: domain.CategoryDailyLiving, Amount: 100_00, ServiceDate: "2026-05-01"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := env.svc.Claims.Approve(as(app.RoleAgencyStaff, ""), c.ID, "verified invoice"); err != nil {
			t.Fatal(err)
		}
		provider := app.AuditEvent{Actor: "sub-provider-PR1", Username: "provider", Roles: []app.Role{app.RoleProvider}, Action: app.ActionSubmitClaim}
		staff := app.AuditEvent{Actor: "sub-agency_staff", Username: "agency_staff", Roles: []app.Role{app.RoleAgencyStaff}, Action: app.ActionApproveClaim}
		claim := app.Resource{ParticipantID: "P1", PlanID: "PL1", ProviderID: "PR1", Category: domain.CategoryDailyLiving, ClaimID: c.ID}
		want := []app.AuditEvent{
			event(provider, app.Resource{ParticipantID: "P1", PlanID: "PL1", Category: domain.CategoryDailyLiving}, "allow", ""),
			event(provider, claim, "submitted", ""),
			event(staff, app.Resource{ClaimID: c.ID}, "allow", ""),
			event(staff, claim, "approved", "verified invoice"),
			event(staff, claim, "paid", "pay_"+c.ID),
		}
		if got := env.audit.events; !reflect.DeepEqual(got, want) {
			t.Fatalf("trail:\n%s\nwant:\n%s", eventLines(got), eventLines(want))
		}
	})
}

// A request the role gate refuses never reaches its use case, so the gate
// audits the attempt itself: who, which action, on which record, and the
// role it lacked. What it lets through, or cannot attribute to anyone, is
// not its to audit (the use case audits an allowed request).
func TestRoleGateAuditsWhatItRefuses(t *testing.T) {
	svc, audit, _ := newServices(t)
	if err := svc.RoleGate.Require(as(app.RoleParticipant, "P1"), app.ActionApproveClaim, app.Resource{ClaimID: "C2"}, app.RoleAgencyStaff); !errors.Is(err, app.ErrForbidden) {
		t.Fatalf("want ErrForbidden, got %v", err)
	}
	if err := svc.RoleGate.Require(as(app.RoleProvider, "PR1"), app.ActionRaiseSupportRequest, app.Resource{PlanID: "PL1"}, app.RoleParticipant, app.RoleNominee); !errors.Is(err, app.ErrForbidden) {
		t.Fatalf("want ErrForbidden, got %v", err)
	}
	want := []app.AuditEvent{
		{Actor: "sub-participant-P1", Username: "participant", Roles: []app.Role{app.RoleParticipant}, Action: app.ActionApproveClaim,
			Resource: app.Resource{ClaimID: "C2"}, Outcome: "deny", Detail: "requires role agency_staff"},
		{Actor: "sub-provider-PR1", Username: "provider", Roles: []app.Role{app.RoleProvider}, Action: app.ActionRaiseSupportRequest,
			Resource: app.Resource{PlanID: "PL1"}, Outcome: "deny", Detail: "requires role participant or nominee"},
	}
	if !reflect.DeepEqual(audit.events, want) {
		t.Fatalf("audit:\n%s\nwant:\n%s", eventLines(audit.events), eventLines(want))
	}
	audit.reset()
	if err := svc.RoleGate.Require(as(app.RoleAgencyStaff, ""), app.ActionApproveClaim, app.Resource{ClaimID: "C2"}, app.RoleAgencyStaff); err != nil {
		t.Fatalf("staff must pass: %v", err)
	}
	if err := svc.RoleGate.Require(context.Background(), app.ActionApproveClaim, app.Resource{ClaimID: "C2"}, app.RoleAgencyStaff); !errors.Is(err, app.ErrUnauthenticated) {
		t.Fatalf("no caller: want ErrUnauthenticated, got %v", err)
	}
	if len(audit.events) != 0 {
		t.Fatalf("passed and anonymous requests are not the gate's to audit: %+v", audit.events)
	}
}

// The ids in a refusal come from the refused caller, so every one of them is
// bounded: a request body's worth of "id" must not reach the audit trail.
// Every string field of Resource but the enum Category is an id here, so a
// new id field fails this test until it is bounded too.
func TestRoleGateBoundsTheIDsOfARefusal(t *testing.T) {
	tests := []struct{ name, id, want string }{
		{"64 bytes are kept", strings.Repeat("x", 64), strings.Repeat("x", 64)},
		{"65 bytes are cut and marked", strings.Repeat("x", 65), strings.Repeat("x", 64) + "…"},
		{"a cut never splits a character", strings.Repeat("€", 40), strings.Repeat("€", 21) + "…"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc, audit, _ := newServices(t)
			var res app.Resource
			ids := idFields(&res)
			for _, f := range ids {
				f.SetString(tc.id)
			}
			_ = svc.RoleGate.Require(as(app.RoleParticipant, "P1"), app.ActionApproveClaim, res, app.RoleAgencyStaff)
			if len(audit.events) != 1 {
				t.Fatalf("want one refusal, got %d events", len(audit.events))
			}
			got := audit.events[0].Resource
			for name, f := range idFields(&got) {
				if s := f.String(); s != tc.want || !utf8.ValidString(s) {
					t.Errorf("%s = %q (%d bytes), want %q", name, s, len(s), tc.want)
				}
			}
			if len(ids) < 6 || got.Category != "" {
				t.Fatalf("found %d id fields, category %q: the test no longer sees Resource's ids", len(ids), got.Category)
			}
		})
	}
}

// idFields returns the settable id fields of res by name: every string field
// but the enum Category.
func idFields(res *app.Resource) map[string]reflect.Value {
	v := reflect.ValueOf(res).Elem()
	out := map[string]reflect.Value{}
	for i := range v.NumField() {
		if f := v.Type().Field(i); f.Type.Kind() == reflect.String && f.Name != "Category" {
			out[f.Name] = v.Field(i)
		}
	}
	return out
}

// event returns base with the record, outcome and detail of one audit line.
func event(base app.AuditEvent, res app.Resource, outcome, detail string) app.AuditEvent {
	base.Resource, base.Outcome, base.Detail = res, outcome, detail
	return base
}

func eventLines(events []app.AuditEvent) string {
	var b strings.Builder
	for _, e := range events {
		fmt.Fprintf(&b, "  %+v\n", e)
	}
	return b.String()
}

// paymentWriteFails fails the next n writes of a payment result (an update
// of an APPROVED claim) and passes everything else through.
type paymentWriteFails struct {
	store
	n atomic.Int64
}

func (f *paymentWriteFails) UpdateClaim(ctx context.Context, c domain.Claim, from domain.ClaimStatus) error {
	if from == domain.ClaimApproved && f.n.Add(-1) >= 0 {
		return errors.New("connection reset by peer")
	}
	return f.store.UpdateClaim(ctx, c, from)
}

// A blip while storing the processor's answer must not lose it: the write is
// retried once.
func TestPaymentResultWriteIsRetriedOnce(t *testing.T) {
	forEachStore(t, func(t *testing.T, env0 testEnv) {
		st := &paymentWriteFails{store: env0.store}
		st.n.Store(1)
		gw := &fakeGateway{}
		env := newEnvWith(t, st, gw, nil)
		c, err := env.svc.Claims.Approve(as(app.RoleAgencyStaff, ""), "C2", "verified invoice")
		if err != nil {
			t.Fatalf("one failed write must be retried, got %v", err)
		}
		stored, _ := env0.store.GetClaim(context.Background(), "C2")
		if c.Status != domain.ClaimPaid || stored.Status != domain.ClaimPaid || stored.Payment == nil || stored.Payment.Ref != "pay_C2" {
			t.Fatalf("claim = %+v, stored = %+v", c, stored)
		}
		if n := len(gw.requests()); n != 1 {
			t.Fatalf("the processor must be asked once, got %d", n)
		}
	})
}

// When storing the processor's answer keeps failing, its payment reference
// must still be on record (audit trail and error), or nobody could tell the
// money moved.
func TestUnrecordedPaymentKeepsTheProcessorsReference(t *testing.T) {
	forEachStore(t, func(t *testing.T, env0 testEnv) {
		st := &paymentWriteFails{store: env0.store}
		st.n.Store(2)
		env := newEnvWith(t, st, &fakeGateway{}, nil)
		_, err := env.svc.Claims.Approve(as(app.RoleAgencyStaff, ""), "C2", "verified invoice")
		if !errors.Is(err, app.ErrPaymentNotRecorded) || !strings.Contains(err.Error(), "pay_C2") {
			t.Fatalf("the error must be app.ErrPaymentNotRecorded carrying the processor's reference, got %v", err)
		}
		if !env.audit.has(app.ActionApproveClaim, "paid", "pay_C2") {
			t.Fatalf("the payment must be audited before the write, got %+v", env.audit.events)
		}
	})
}

// paymentWriteCommitsThenFails stores the first payment result but reports
// a failure, as when a commit succeeds and its acknowledgement is lost.
type paymentWriteCommitsThenFails struct {
	store
	tripped atomic.Bool
}

func (f *paymentWriteCommitsThenFails) UpdateClaim(ctx context.Context, c domain.Claim, from domain.ClaimStatus) error {
	if from == domain.ClaimApproved && f.tripped.CompareAndSwap(false, true) {
		if err := f.store.UpdateClaim(ctx, c, from); err != nil {
			return err
		}
		return errors.New("connection reset by peer")
	}
	return f.store.UpdateClaim(ctx, c, from)
}

// paymentWriteConflicts has another writer store a different payment result
// before failing ours, so the retry finds the claim in a state that is not
// this payment's.
type paymentWriteConflicts struct {
	store
	tripped atomic.Bool
}

func (f *paymentWriteConflicts) UpdateClaim(ctx context.Context, c domain.Claim, from domain.ClaimStatus) error {
	if from == domain.ClaimApproved && c.Payment != nil && f.tripped.CompareAndSwap(false, true) {
		other := c
		other.Payment = &domain.ClaimPayment{Status: c.Payment.Status, Ref: "pay_OTHER"}
		if err := f.store.UpdateClaim(ctx, other, from); err != nil {
			return err
		}
		return errors.New("connection reset by peer")
	}
	return f.store.UpdateClaim(ctx, c, from)
}

// When the processor paid but its result cannot be stored because the claim
// holds a different payment, the money moved and is not on record: a server
// failure that names the payment, never a validation error that reads as
// "try again".
func TestAPaymentThatCannotBeRecordedIsAServerFailure(t *testing.T) {
	forEachStore(t, func(t *testing.T, env0 testEnv) {
		st := &paymentWriteConflicts{store: env0.store}
		env := newEnvWith(t, st, &fakeGateway{}, nil)
		_, err := env.svc.Claims.Approve(as(app.RoleAgencyStaff, ""), "C2", "verified invoice")
		if !errors.Is(err, app.ErrPaymentNotRecorded) {
			t.Fatalf("want app.ErrPaymentNotRecorded, got %v", err)
		}
		var ve *domain.ValidationError
		if errors.As(err, &ve) {
			t.Fatalf("an unrecorded payment must not surface as a validation error: %v", err)
		}
		if !strings.Contains(err.Error(), "pay_C2") || !strings.Contains(err.Error(), "C2") {
			t.Fatalf("the error must name the claim and the processor's reference, got %v", err)
		}
	})
}

// When the first write of a payment result was stored but reported as
// failed, the retry finds the claim already paid: that is success, not an
// error for staff to puzzle over.
func TestAStoredPaymentResultIsNotReportedAsAFailure(t *testing.T) {
	forEachStore(t, func(t *testing.T, env0 testEnv) {
		st := &paymentWriteCommitsThenFails{store: env0.store}
		env := newEnvWith(t, st, &fakeGateway{}, nil)
		c, err := env.svc.Claims.Approve(as(app.RoleAgencyStaff, ""), "C2", "verified invoice")
		if err != nil {
			t.Fatalf("the payment result was stored; want success, got %v", err)
		}
		stored, _ := env0.store.GetClaim(context.Background(), "C2")
		if c.Status != domain.ClaimPaid || stored.Status != domain.ClaimPaid || stored.Payment == nil || stored.Payment.Ref != "pay_C2" {
			t.Fatalf("claim = %+v, stored = %+v", c, stored)
		}
	})
}
