package app_test

import (
	"context"
	"sync"
	"testing"

	"github.com/eliteGoblin/eks-gitops-demo/internal/adapter/memory"
	"github.com/eliteGoblin/eks-gitops-demo/internal/app"
	"github.com/eliteGoblin/eks-gitops-demo/internal/domain"
)

func seededMemory() *memory.Store {
	st := memory.New()
	memory.Seed(st)
	return st
}

// withCancel gives a caller's context (from as) the cancellation of parent.
func withCancel(parent, caller context.Context) context.Context {
	pr, _ := app.PrincipalFrom(caller)
	return app.WithPrincipal(parent, pr)
}

func raiseTherapyWithProvider(t *testing.T, svc *app.Services, ctx context.Context, planID string, amount domain.Money, provider string) domain.SupportRequest {
	t.Helper()
	r, err := svc.SupportRequests.Raise(ctx, app.RaiseSupportRequestInput{
		PlanID: planID, Category: domain.CategoryTherapy, Description: "Weekly physiotherapy", EstimatedAmount: amount,
		PreferredProviderID: provider,
	})
	if err != nil {
		t.Fatalf("raise: %v", err)
	}
	return r
}

// fakeGateway plays payments-svc: it records every request and answers with
// err, or with a receipt whose id derives from the idempotency key.
type fakeGateway struct {
	mu     sync.Mutex
	calls  []app.PaymentRequest
	err    error
	onCall func() // runs before answering, e.g. to cancel the caller's context
}

func (g *fakeGateway) RequestPayment(ctx context.Context, req app.PaymentRequest) (app.PaymentReceipt, error) {
	g.mu.Lock()
	g.calls = append(g.calls, req)
	err, onCall := g.err, g.onCall
	g.mu.Unlock()
	if onCall != nil {
		onCall()
	}
	if ctx.Err() != nil {
		return app.PaymentReceipt{}, ctx.Err()
	}
	if err != nil {
		return app.PaymentReceipt{}, err
	}
	return app.PaymentReceipt{PaymentID: "pay_" + req.IdempotencyKey, Status: "ACCEPTED"}, nil
}

func (g *fakeGateway) requests() []app.PaymentRequest {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]app.PaymentRequest(nil), g.calls...)
}

// fakeEvents is an in-process event bus: it records what was published and
// delivers it to matching subscribers until their context ends.
type fakeEvents struct {
	mu        sync.Mutex
	published []app.Event
	subs      map[*fakeSub]struct{}
}

type fakeSub struct {
	kind   app.EventKind
	planID string
	ch     chan app.Event
}

func newFakeEvents() *fakeEvents { return &fakeEvents{subs: map[*fakeSub]struct{}{}} }

func (f *fakeEvents) Publish(_ context.Context, e app.Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.published = append(f.published, e)
	for s := range f.subs {
		if s.kind == e.Kind && s.planID == e.PlanID {
			s.ch <- e
		}
	}
}

func (f *fakeEvents) Subscribe(ctx context.Context, kind app.EventKind, planID string) (<-chan app.Event, error) {
	s := &fakeSub{kind: kind, planID: planID, ch: make(chan app.Event, 64)}
	f.mu.Lock()
	f.subs[s] = struct{}{}
	f.mu.Unlock()
	go func() {
		<-ctx.Done()
		f.mu.Lock()
		defer f.mu.Unlock()
		delete(f.subs, s)
		close(s.ch)
	}()
	return s.ch, nil
}

func (f *fakeEvents) kinds() []app.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]app.Event(nil), f.published...)
}
