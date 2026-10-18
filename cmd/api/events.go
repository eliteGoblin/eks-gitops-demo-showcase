package main

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"github.com/eliteGoblin/eks-gitops-demo/internal/adapter/events"
	"github.com/eliteGoblin/eks-gitops-demo/internal/adapter/postgres"
	"github.com/eliteGoblin/eks-gitops-demo/internal/app"
)

// notifyingStore can carry change events between replicas (Postgres
// LISTEN/NOTIFY).
type notifyingStore interface {
	events.Notifier
	Connect(ctx context.Context) (*pgx.Conn, error)
}

// Without it, a signature drift would silently keep events in one replica.
var _ notifyingStore = (*postgres.Store)(nil)

// openEvents returns the change-event bus behind subscriptions. With the
// memory store there is one replica and events stay in process; with
// Postgres every replica publishes with NOTIFY and listens on one dedicated
// connection, reconnecting until ctx ends.
func openEvents(ctx context.Context, store repositories, log *slog.Logger) app.Events {
	hub := events.NewHub(log)
	pg, ok := store.(notifyingStore)
	if !ok {
		return hub
	}
	listener := &events.Listener{Connect: events.PGConnect(pg.Connect), Hub: hub, Log: log}
	go func() { _ = listener.Run(ctx) }() // returns when ctx ends; it logs its own reconnects
	return events.NewPostgresBus(hub, pg, log)
}
