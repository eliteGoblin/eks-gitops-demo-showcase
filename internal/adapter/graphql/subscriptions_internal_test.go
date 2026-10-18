package graphql

import (
	"context"
	"errors"
	"testing"

	"github.com/eliteGoblin/eks-gitops-demo/internal/app"
)

func onConnection(slots int) (context.Context, *connection) {
	conn := &connection{slots: make(chan struct{}, slots)}
	return context.WithValue(context.Background(), connectionKey{}, conn), conn
}

// untilDone is a watch that streams nothing and ends with its context.
func untilDone(ctx context.Context, _ string) (<-chan int, error) {
	ch := make(chan int)
	go func() {
		<-ctx.Done()
		close(ch)
	}()
	return ch, nil
}

func box(v int) *int { return &v }

func TestSubscriptionSlotIsFreeOnceTheStreamEnds(t *testing.T) {
	base, _ := onConnection(1)
	ctx, cancel := context.WithCancel(base)
	out, err := subscribe(ctx, "PL1", untilDone, box)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := subscribe(base, "PL1", untilDone, box); err == nil {
		t.Fatal("a second subscription must not fit one slot")
	}
	cancel()
	for range out {
	}
	if _, err := subscribe(base, "PL1", untilDone, box); err != nil {
		t.Fatalf("the slot must be free once the stream has ended: %v", err)
	}
}

func TestRefusedWatchDoesNotHoldASlot(t *testing.T) {
	ctx, conn := onConnection(1)
	forbidden := func(context.Context, string) (<-chan int, error) { return nil, app.ErrForbidden }
	if _, err := subscribe(ctx, "PL1", forbidden, box); !errors.Is(err, app.ErrForbidden) {
		t.Fatalf("err = %v", err)
	}
	if len(conn.slots) != 0 {
		t.Fatal("a refused watch kept its slot")
	}
}

func TestSubscribeNeedsAConnection(t *testing.T) {
	if _, err := subscribe(context.Background(), "PL1", untilDone, box); err == nil {
		t.Fatal("want an error outside a WebSocket connection")
	}
}

func TestOriginHosts(t *testing.T) {
	got := originHosts([]string{"https://app.example", "http://localhost:5173", "not a url", ""})
	if len(got) != 2 || got[0] != "app.example" || got[1] != "localhost:5173" {
		t.Fatalf("originHosts = %q", got)
	}
}
