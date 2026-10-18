package events

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/eliteGoblin/eks-gitops-demo/internal/app"
)

var discard = slog.New(slog.DiscardHandler)

func receive(t *testing.T, ch <-chan app.Event) app.Event {
	t.Helper()
	select {
	case e, ok := <-ch:
		if !ok {
			t.Fatal("stream closed")
		}
		return e
	case <-time.After(2 * time.Second):
		t.Fatal("no event within 2s")
	}
	return app.Event{}
}

func nothing(t *testing.T, ch <-chan app.Event) {
	t.Helper()
	select {
	case e := <-ch:
		t.Fatalf("unexpected event %+v", e)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestHubDeliversOnlyMatchingEvents(t *testing.T) {
	h := NewHub(discard)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	claimsPL1, _ := h.Subscribe(ctx, app.ClaimChanged, "PL1")
	requestsPL1, _ := h.Subscribe(ctx, app.SupportRequestChanged, "PL1")
	claimsPL2, _ := h.Subscribe(ctx, app.ClaimChanged, "PL2")

	h.Publish(ctx, app.Event{Kind: app.ClaimChanged, ID: "C1", PlanID: "PL1"})
	if e := receive(t, claimsPL1); e.ID != "C1" {
		t.Fatalf("got %+v", e)
	}
	nothing(t, requestsPL1)
	nothing(t, claimsPL2)
}

func TestHubClosesTheStreamWhenTheContextEnds(t *testing.T) {
	h := NewHub(discard)
	ctx, cancel := context.WithCancel(context.Background())
	ch, _ := h.Subscribe(ctx, app.ClaimChanged, "PL1")
	cancel()
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("no event was published")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the stream must close when the context ends")
	}
	h.Dispatch(app.Event{Kind: app.ClaimChanged, ID: "C1", PlanID: "PL1"}) // must not panic on a closed stream
}

// A subscriber that stops reading loses events once its buffer is full; it
// never stalls delivery to the others.
func TestSlowSubscriberDoesNotBlockOthers(t *testing.T) {
	h := NewHub(discard)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, _ = h.Subscribe(ctx, app.ClaimChanged, "PL1") // never read
	fast, _ := h.Subscribe(ctx, app.ClaimChanged, "PL1")
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range subscriberBuffer * 4 {
			h.Dispatch(app.Event{Kind: app.ClaimChanged, ID: string(rune('a' + i%26)), PlanID: "PL1"})
			receive(t, fast)
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a stalled subscriber blocked delivery")
	}
	if h.Dropped() == 0 {
		t.Fatal("the stalled subscriber's overflow must be counted as dropped")
	}
}

// Dispatching while subscribers come and go is race-free (run with -race).
func TestHubIsSafeUnderConcurrency(t *testing.T) {
	h := NewHub(discard)
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			ctx, cancel := context.WithCancel(context.Background())
			ch, _ := h.Subscribe(ctx, app.ClaimChanged, "PL1")
			go func() {
				for range ch { //nolint:revive // drain until closed
				}
			}()
			time.Sleep(time.Millisecond)
			cancel()
		})
		wg.Go(func() {
			for range 50 {
				h.Dispatch(app.Event{Kind: app.ClaimChanged, ID: "C1", PlanID: "PL1"})
			}
		})
	}
	wg.Wait()
}
