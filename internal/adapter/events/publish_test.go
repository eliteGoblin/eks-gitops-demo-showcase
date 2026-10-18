package events_test

import (
	"context"
	"log/slog"
	"testing"

	"github.com/eliteGoblin/eks-gitops-demo/internal/adapter/events"
	"github.com/eliteGoblin/eks-gitops-demo/internal/app"
)

// recordingNotifier remembers the state of the context it was called with.
type recordingNotifier struct {
	called      bool
	errAtCall   error
	hasDeadline bool
}

func (n *recordingNotifier) Notify(ctx context.Context, _, _ string) error {
	n.called, n.errAtCall = true, ctx.Err()
	_, n.hasDeadline = ctx.Deadline()
	return nil
}

// The change being announced has already committed: a client that has
// already gone (its request context cancelled) must not stop the
// announcement, but the announcement must be bounded by its own timeout.
func TestPublishOutlivesTheCaller(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	n := &recordingNotifier{}
	bus := events.NewPostgresBus(events.NewHub(log), n, log)
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	bus.Publish(gone, app.Event{Kind: app.ClaimChanged, ID: "C1", PlanID: "PL1"})
	if !n.called || n.errAtCall != nil {
		t.Fatalf("the NOTIFY must run on a live context, got called=%v err=%v", n.called, n.errAtCall)
	}
	if !n.hasDeadline {
		t.Fatal("the NOTIFY must be bounded by its own timeout")
	}
}
