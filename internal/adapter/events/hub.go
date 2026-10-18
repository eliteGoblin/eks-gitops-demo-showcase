// Package events carries change announcements (app.Event) to subscribers.
// Hub fans events out within one replica. With Postgres, PostgresBus sends
// every event to all replicas with NOTIFY, and each replica's Listener holds
// one LISTEN session that feeds its Hub.
package events

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/eliteGoblin/eks-gitops-demo/internal/app"
)

// subscriberBuffer is how many undelivered events a subscriber may lag behind
// before it starts losing them.
const subscriberBuffer = 16

// Hub fans events out to this replica's subscribers. It implements app.Events
// on its own (one replica, e.g. the in-memory store).
type Hub struct {
	mu      sync.Mutex
	subs    map[*subscriber]struct{}
	dropped atomic.Int64
	log     *slog.Logger
}

type subscriber struct {
	kind   app.EventKind
	planID string
	ch     chan app.Event
}

var _ app.Events = (*Hub)(nil)

// NewHub returns a hub with no subscribers.
func NewHub(log *slog.Logger) *Hub {
	return &Hub{subs: map[*subscriber]struct{}{}, log: log}
}

// Subscribe streams the events of kind on planID until ctx ends, then closes
// the channel.
func (h *Hub) Subscribe(ctx context.Context, kind app.EventKind, planID string) (<-chan app.Event, error) {
	s := &subscriber{kind: kind, planID: planID, ch: make(chan app.Event, subscriberBuffer)}
	h.mu.Lock()
	h.subs[s] = struct{}{}
	h.mu.Unlock()
	go func() {
		<-ctx.Done()
		h.mu.Lock()
		defer h.mu.Unlock()
		delete(h.subs, s)
		close(s.ch) // under the lock that Dispatch sends under: never a send on a closed channel
	}()
	return s.ch, nil
}

// Dispatch delivers e to this replica's matching subscribers. A subscriber
// whose buffer is full loses the event (counted and logged) instead of
// stalling everyone else; it catches up with the next change, since every
// event makes it re-read the current record.
func (h *Hub) Dispatch(e app.Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs {
		if s.kind != e.Kind || s.planID != e.PlanID {
			continue
		}
		select {
		case s.ch <- e:
		default:
			h.dropped.Add(1)
			h.log.Warn("subscriber is not keeping up; event dropped", "kind", e.Kind, "plan", e.PlanID)
		}
	}
}

// Publish dispatches within this replica.
func (h *Hub) Publish(_ context.Context, e app.Event) { h.Dispatch(e) }

// Dropped reports how many deliveries were lost to full subscriber buffers.
func (h *Hub) Dropped() int64 { return h.dropped.Load() }
