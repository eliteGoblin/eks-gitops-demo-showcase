package events

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/eliteGoblin/eks-gitops-demo/internal/app"
)

// Channel is the NOTIFY channel every replica listens on.
const Channel = "demo_events"

// wire is an event on the channel: identifiers only, well under NOTIFY's
// 8000-byte payload limit.
type wire struct {
	Kind   app.EventKind `json:"k"`
	ID     string        `json:"id"`
	PlanID string        `json:"plan"`
}

func toWire(e app.Event) wire   { return wire{Kind: e.Kind, ID: e.ID, PlanID: e.PlanID} }
func (w wire) event() app.Event { return app.Event{Kind: w.Kind, ID: w.ID, PlanID: w.PlanID} }

// Notifier sends a NOTIFY; the Postgres store implements it with its pool.
type Notifier interface {
	Notify(ctx context.Context, channel, payload string) error
}

// PostgresBus publishes to every replica with NOTIFY and subscribes through
// this replica's hub, which a Listener feeds. A replica receives its own
// events through its Listener too, so each subscriber sees an event once.
type PostgresBus struct {
	hub      *Hub
	notifier Notifier
	log      *slog.Logger
}

var _ app.Events = (*PostgresBus)(nil)

// NewPostgresBus returns a bus over hub and notifier.
func NewPostgresBus(hub *Hub, notifier Notifier, log *slog.Logger) *PostgresBus {
	return &PostgresBus{hub: hub, notifier: notifier, log: log}
}

// publishTimeout bounds one NOTIFY.
const publishTimeout = 2 * time.Second

// Publish NOTIFYs the event. A failure is logged, not returned: the change
// it announces has already committed, and subscribers are live views. The
// NOTIFY is detached from the caller's cancellation (a client that has gone
// must not suppress the announcement) and bounded by its own timeout.
func (b *PostgresBus) Publish(ctx context.Context, e app.Event) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), publishTimeout)
	defer cancel()
	payload, err := json.Marshal(toWire(e))
	if err == nil {
		err = b.notifier.Notify(ctx, Channel, string(payload))
	}
	if err != nil {
		b.log.WarnContext(ctx, "could not announce a change", "kind", e.Kind, "plan", e.PlanID, "err", err)
	}
}

// Subscribe streams this replica's share of the events (see Hub.Subscribe).
func (b *PostgresBus) Subscribe(ctx context.Context, kind app.EventKind, planID string) (<-chan app.Event, error) {
	return b.hub.Subscribe(ctx, kind, planID)
}

// Conn is the part of a dedicated connection a LISTEN session uses.
type Conn interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	WaitForNotification(ctx context.Context) (*pgconn.Notification, error)
	Close(ctx context.Context) error
}

// PGConnect adapts a function that opens a *pgx.Conn to Listener.Connect.
func PGConnect(connect func(ctx context.Context) (*pgx.Conn, error)) func(ctx context.Context) (Conn, error) {
	return func(ctx context.Context) (Conn, error) {
		conn, err := connect(ctx)
		if err != nil {
			return nil, err // never a typed nil inside the interface
		}
		return conn, nil
	}
}

// Listener keeps one LISTEN session per replica and feeds the hub. When the
// session drops, it reconnects with capped exponential backoff until ctx ends.
// Events NOTIFYed while it is disconnected are not replayed (subscriptions
// show changes as they happen; queries remain the source of truth).
type Listener struct {
	Connect    func(ctx context.Context) (Conn, error)
	Hub        *Hub
	Log        *slog.Logger
	MinBackoff time.Duration // default 500ms
	MaxBackoff time.Duration // default 30s
}

// Run listens until ctx ends; it returns nil then.
func (l *Listener) Run(ctx context.Context) error {
	minWait, maxWait := l.MinBackoff, l.MaxBackoff
	if minWait <= 0 {
		minWait = 500 * time.Millisecond
	}
	if maxWait <= 0 {
		maxWait = 30 * time.Second
	}
	maxWait = max(maxWait, minWait)
	wait := minWait
	for {
		connected, err := l.listenOnce(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if connected {
			wait = minWait // the session worked: start over from the shortest pause
		}
		l.Log.WarnContext(ctx, "event listener disconnected; reconnecting", "err", err, "in", wait)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
		wait = min(wait*2, maxWait)
	}
}

// listenOnce runs one session; connected reports whether LISTEN succeeded.
func (l *Listener) listenOnce(ctx context.Context) (connected bool, err error) {
	conn, err := l.Connect(ctx)
	if err != nil {
		return false, fmt.Errorf("connect: %w", err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	if _, err := conn.Exec(ctx, "LISTEN "+pgx.Identifier{Channel}.Sanitize()); err != nil {
		return false, fmt.Errorf("listen: %w", err)
	}
	l.Log.InfoContext(ctx, "event listener connected", "channel", Channel)
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return true, err
		}
		var w wire
		if err := json.Unmarshal([]byte(n.Payload), &w); err != nil {
			l.Log.WarnContext(ctx, "ignoring a malformed event", "err", err)
			continue
		}
		if w.ID == "" || w.PlanID == "" {
			l.Log.WarnContext(ctx, "ignoring an event without an id or plan")
			continue
		}
		l.Hub.Dispatch(w.event())
	}
}
