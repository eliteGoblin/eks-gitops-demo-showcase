package events

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/eliteGoblin/eks-gitops-demo/internal/app"
)

// fakeConn plays a LISTEN session: it hands out queued notifications, then
// fails with err (a dropped connection) or waits for ctx.
type fakeConn struct {
	mu       sync.Mutex
	listened []string
	notes    chan *pgconn.Notification
	err      error
	closed   atomic.Bool
}

func (c *fakeConn) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.listened = append(c.listened, sql)
	return pgconn.CommandTag{}, nil
}

func (c *fakeConn) WaitForNotification(ctx context.Context) (*pgconn.Notification, error) {
	select {
	case n, ok := <-c.notes:
		if !ok {
			return nil, c.err
		}
		return n, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *fakeConn) Close(context.Context) error { c.closed.Store(true); return nil }

func note(t *testing.T, e app.Event) *pgconn.Notification {
	t.Helper()
	b, err := json.Marshal(toWire(e))
	if err != nil {
		t.Fatal(err)
	}
	return &pgconn.Notification{Channel: Channel, Payload: string(b)}
}

func TestListenerFeedsTheHubAndSkipsBadPayloads(t *testing.T) {
	hub := NewHub(discard)
	conn := &fakeConn{notes: make(chan *pgconn.Notification, 4)}
	conn.notes <- &pgconn.Notification{Channel: Channel, Payload: "not json"}
	conn.notes <- note(t, app.Event{Kind: app.ClaimChanged, ID: "C9", PlanID: "PL1"})
	l := &Listener{Connect: func(context.Context) (Conn, error) { return conn, nil }, Hub: hub, Log: discard}

	ctx, cancel := context.WithCancel(context.Background())
	sub, _ := hub.Subscribe(ctx, app.ClaimChanged, "PL1")
	done := make(chan error, 1)
	go func() { done <- l.Run(ctx) }()
	if e := receive(t, sub); e.ID != "C9" {
		t.Fatalf("got %+v", e)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run must end cleanly when ctx ends, got %v", err)
	}
	if !conn.closed.Load() || len(conn.listened) != 1 || conn.listened[0] != `LISTEN "demo_events"` {
		t.Fatalf("listened %q, closed %v", conn.listened, conn.closed.Load())
	}
}

// A dropped connection (or a failed connect) is retried with backoff, and the
// new session feeds the hub again.
func TestListenerReconnectsWithBackoff(t *testing.T) {
	hub := NewHub(discard)
	var connects atomic.Int64
	broken := &fakeConn{notes: make(chan *pgconn.Notification), err: errors.New("connection reset")}
	close(broken.notes) // the session drops at once
	healthy := &fakeConn{notes: make(chan *pgconn.Notification, 1)}
	healthy.notes <- note(t, app.Event{Kind: app.SupportRequestChanged, ID: "SR1", PlanID: "PL3"})
	l := &Listener{
		Connect: func(context.Context) (Conn, error) {
			switch connects.Add(1) {
			case 1:
				return nil, errors.New("database starting")
			case 2:
				return broken, nil
			default:
				return healthy, nil
			}
		},
		Hub: hub, Log: discard, MinBackoff: time.Millisecond, MaxBackoff: 5 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sub, _ := hub.Subscribe(ctx, app.SupportRequestChanged, "PL3")
	go func() { _ = l.Run(ctx) }()
	if e := receive(t, sub); e.ID != "SR1" {
		t.Fatalf("got %+v", e)
	}
	if n := connects.Load(); n < 3 {
		t.Fatalf("connects = %d; a failed connect and a dropped session must both be retried", n)
	}
	if !broken.closed.Load() {
		t.Fatal("the dropped session must be closed")
	}
}

func TestBusPublishesWithNotify(t *testing.T) {
	var got []string
	n := notifierFunc(func(_ context.Context, channel, payload string) error {
		got = append(got, channel+" "+payload)
		return nil
	})
	b := NewPostgresBus(NewHub(discard), n, discard)
	b.Publish(context.Background(), app.Event{Kind: app.ClaimChanged, ID: "C1", PlanID: "PL1"})
	if len(got) != 1 || got[0] != `demo_events {"k":"claim","id":"C1","plan":"PL1"}` {
		t.Fatalf("notified %q", got)
	}
}

// A failed NOTIFY is logged, never returned: the change already committed.
func TestBusPublishFailureIsNotFatal(t *testing.T) {
	b := NewPostgresBus(NewHub(discard), notifierFunc(func(context.Context, string, string) error {
		return errors.New("connection refused")
	}), discard)
	b.Publish(context.Background(), app.Event{Kind: app.ClaimChanged, ID: "C1", PlanID: "PL1"}) // must not panic
}

type notifierFunc func(ctx context.Context, channel, payload string) error

func (f notifierFunc) Notify(ctx context.Context, channel, payload string) error {
	return f(ctx, channel, payload)
}
