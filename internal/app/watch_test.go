package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/eliteGoblin/eks-gitops-demo/internal/app"
	"github.com/eliteGoblin/eks-gitops-demo/internal/domain"
)

func next[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v, ok := <-ch:
		if !ok {
			t.Fatal("stream closed")
		}
		return v
	case <-time.After(2 * time.Second):
		t.Fatal("no update within 2s")
	}
	var zero T
	return zero
}

func none[T any](t *testing.T, ch <-chan T) {
	t.Helper()
	select {
	case v := <-ch:
		t.Fatalf("unexpected update %+v", v)
	case <-time.After(100 * time.Millisecond):
	}
}

// Subscribing needs read access to the plan; "no such plan" and "not
// permitted" look the same.
func TestWatchRequiresReadingThePlan(t *testing.T) {
	env := newEnvWith(t, seededMemory(), nil, newFakeEvents())
	for name, tc := range map[string]struct {
		ctx  context.Context
		plan string
	}{
		"another participant's plan": {as(app.RoleParticipant, "P3"), "PL1"},
		"provider not booked":        {as(app.RoleProvider, "PR2"), "PL1"},
		"no such plan":               {as(app.RoleAgencyStaff, ""), "PL404"},
	} {
		if _, err := env.svc.Claims.Watch(tc.ctx, tc.plan); !errors.Is(err, app.ErrForbidden) {
			t.Errorf("claims, %s: want ErrForbidden, got %v", name, err)
		}
		if _, err := env.svc.SupportRequests.Watch(tc.ctx, tc.plan); !errors.Is(err, app.ErrForbidden) {
			t.Errorf("support requests, %s: want ErrForbidden, got %v", name, err)
		}
	}
	if _, err := env.svc.Claims.Watch(context.Background(), "PL1"); !errors.Is(err, app.ErrUnauthenticated) {
		t.Errorf("anonymous watch: want ErrUnauthenticated, got %v", err)
	}
}

// Each change is re-read with the subscriber's access: a provider following a
// plan sees its own claims change, never another provider's.
func TestWatchClaimsReauthorizesEveryUpdate(t *testing.T) {
	ev := newFakeEvents()
	env := newEnvWith(t, seededMemory(), nil, ev)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	watch := func(base context.Context) <-chan domain.Claim {
		ch, err := env.svc.Claims.Watch(withCancel(ctx, base), "PL1")
		if err != nil {
			t.Fatal(err)
		}
		return ch
	}
	participant := watch(as(app.RoleParticipant, "P1"))
	provider := watch(as(app.RoleProvider, "PR1"))

	// PR2 gets a booking on PL1 (support request approved), then claims on it.
	r := raiseTherapyWithProvider(t, env.svc, as(app.RoleParticipant, "P1"), "PL1", 100_00, "PR2")
	if _, err := env.svc.SupportRequests.Decide(as(app.RoleAgencyStaff, ""), app.DecideSupportRequestInput{ID: r.ID, Approve: true, Reason: "ok"}); err != nil {
		t.Fatal(err)
	}
	pr2Claim, err := env.svc.Claims.Submit(as(app.RoleProvider, "PR2"), app.SubmitClaimInput{PlanID: "PL1", Category: domain.CategoryTherapy, Amount: 50_00, ServiceDate: "2026-05-01"})
	if err != nil {
		t.Fatal(err)
	}
	if got := next(t, participant); got.ID != pr2Claim.ID || got.Status != domain.ClaimSubmitted {
		t.Fatalf("participant update = %+v", got)
	}
	none(t, provider) // PR1 may not read PR2's claim

	if _, err := env.svc.Claims.Approve(as(app.RoleAgencyStaff, ""), "C2", "verified invoice"); err != nil {
		t.Fatal(err)
	}
	if got := next(t, provider); got.ID != "C2" || got.Status != domain.ClaimApproved {
		t.Fatalf("provider update = %+v", got)
	}
	if got := next(t, participant); got.ID != "C2" {
		t.Fatalf("participant update = %+v", got)
	}
}

// The stream ends when the subscriber's context does.
func TestWatchEndsWithTheContext(t *testing.T) {
	env := newEnvWith(t, seededMemory(), nil, newFakeEvents())
	ctx, cancel := context.WithCancel(as(app.RoleAgencyStaff, ""))
	ch, err := env.svc.Claims.Watch(ctx, "PL1")
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("no update was published")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the stream must close when the context ends")
	}
}

// Support requests stream through raise and decision, also re-read per update.
func TestWatchSupportRequests(t *testing.T) {
	ev := newFakeEvents()
	env := newEnvWith(t, seededMemory(), nil, ev)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := env.svc.SupportRequests.Watch(withCancel(ctx, as(app.RoleParticipant, "P1")), "PL1")
	if err != nil {
		t.Fatal(err)
	}
	staffStream, err := env.svc.SupportRequests.Watch(withCancel(ctx, as(app.RoleAgencyStaff, "")), "PL1")
	if err != nil {
		t.Fatal(err)
	}
	r := raiseTherapy(t, env.svc, as(app.RoleParticipant, "P1"), "PL1", 100_00)
	if got := next(t, stream); got.ID != r.ID || got.Status != domain.SupportRequestPending {
		t.Fatalf("raised = %+v", got)
	}
	if got := next(t, staffStream); got.ID != r.ID {
		t.Fatalf("staff sees the raise = %+v", got)
	}
	if _, err := env.svc.SupportRequests.Decide(as(app.RoleAgencyStaff, ""), app.DecideSupportRequestInput{ID: r.ID, Approve: false, Reason: "duplicate"}); err != nil {
		t.Fatal(err)
	}
	if got := next(t, stream); got.ID != r.ID || got.Status != domain.SupportRequestRejected {
		t.Fatalf("decided = %+v", got)
	}
}

// Without an event bus, live updates are unavailable rather than silent.
func TestWatchWithoutEvents(t *testing.T) {
	svc, _, _ := newServices(t)
	if _, err := svc.Claims.Watch(as(app.RoleAgencyStaff, ""), "PL1"); !errors.Is(err, app.ErrEventsUnavailable) {
		t.Fatalf("want ErrEventsUnavailable, got %v", err)
	}
}

// Get re-reads a single support request with the same rules as listing it.
func TestGetSupportRequest(t *testing.T) {
	env := newEnvWith(t, seededMemory(), nil, nil)
	r := raiseTherapy(t, env.svc, as(app.RoleParticipant, "P1"), "PL1", 100_00)
	for name, tc := range map[string]struct {
		ctx     context.Context
		visible bool
	}{
		"the requester":             {as(app.RoleParticipant, "P1"), true},
		"the participant's nominee": {as(app.RoleNominee, "N1"), true},
		"agency staff":              {as(app.RoleAgencyStaff, ""), true},
		"another participant":       {as(app.RoleParticipant, "P3"), false},
		"a provider":                {as(app.RoleProvider, "PR1"), false},
	} {
		got, err := env.svc.SupportRequests.Get(tc.ctx, r.ID)
		if err != nil || (got != nil) != tc.visible {
			t.Errorf("%s: got %v err %v, want visible=%v", name, got, err, tc.visible)
		}
	}
}

// Get re-reads a single claim with the listing rules.
func TestGetClaim(t *testing.T) {
	env := newEnvWith(t, seededMemory(), nil, nil)
	for name, tc := range map[string]struct {
		ctx     context.Context
		id      string
		visible bool
	}{
		"participant, own plan": {as(app.RoleParticipant, "P1"), "C2", true},
		"provider, own claim":   {as(app.RoleProvider, "PR1"), "C2", true},
		"provider, other claim": {as(app.RoleProvider, "PR1"), "C3", false},
		"other participant":     {as(app.RoleParticipant, "P3"), "C2", false},
		"staff":                 {as(app.RoleAgencyStaff, ""), "C3", true},
		"missing":               {as(app.RoleAgencyStaff, ""), "C404", false},
	} {
		got, err := env.svc.Claims.Get(tc.ctx, tc.id)
		if err != nil || (got != nil) != tc.visible {
			t.Errorf("%s: got %v err %v, want visible=%v", name, got, err, tc.visible)
		}
	}
}
