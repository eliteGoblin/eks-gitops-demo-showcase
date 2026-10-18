package app

import (
	"context"
	"errors"
)

// EventKind names what changed.
type EventKind string

const (
	ClaimChanged          EventKind = "claim"
	SupportRequestChanged EventKind = "support_request"
)

// Event announces that a record changed. It carries identifiers only:
// subscribers re-read the record with their own authorization, so an event
// never leaks data and a revoked relationship takes effect on the next update.
type Event struct {
	Kind   EventKind
	ID     string
	PlanID string
}

// EventPublisher announces committed changes to the subscribers on every
// replica. Publishing is best effort (subscriptions are live views, not a
// source of truth): implementations log a failure instead of failing the
// change that already committed.
type EventPublisher interface {
	Publish(ctx context.Context, e Event)
}

// EventSubscriber streams the events of one kind on one plan until ctx ends,
// then closes the channel.
type EventSubscriber interface {
	Subscribe(ctx context.Context, kind EventKind, planID string) (<-chan Event, error)
}

// Events is the event bus the use cases need: they publish what they change
// and let callers follow a plan.
type Events interface {
	EventPublisher
	EventSubscriber
}

// ErrEventsUnavailable is returned by Watch when no event bus is configured.
var ErrEventsUnavailable = errors.New("live updates are not available")

// noEvents is the bus when none is configured: nothing is announced, and
// watching fails explicitly instead of staying silent forever.
type noEvents struct{}

func (noEvents) Publish(context.Context, Event) {}

func (noEvents) Subscribe(context.Context, EventKind, string) (<-chan Event, error) {
	return nil, ErrEventsUnavailable
}

// watch re-reads every announced record through get (which authorizes the
// caller) and streams what the caller may see. A record the caller may not
// see is skipped; a read failure ends the stream, so the client knows to
// resubscribe instead of silently missing updates. The stream ends with ctx.
func watch[T any](ctx context.Context, events <-chan Event, get func(ctx context.Context, id string) (*T, error)) <-chan T {
	out := make(chan T, 1)
	go func() {
		defer close(out)
		for e := range events {
			v, err := get(ctx, e.ID)
			if err != nil {
				return
			}
			if v == nil {
				continue
			}
			select {
			case out <- *v:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}
