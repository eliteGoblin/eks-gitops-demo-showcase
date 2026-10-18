package events_test

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/eliteGoblin/eks-gitops-demo/internal/adapter/events"
	"github.com/eliteGoblin/eks-gitops-demo/internal/adapter/postgres/pgtest"
	"github.com/eliteGoblin/eks-gitops-demo/internal/app"
)

// Two "replicas" on one database: an event one publishes reaches a subscriber
// on the other through LISTEN/NOTIFY, also after its session is killed.
func TestEventsCrossReplicasThroughPostgres(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	storeA, storeB := pgtest.NewStore(t), pgtest.NewStore(t)
	hubB := events.NewHub(log)
	busA := events.NewPostgresBus(events.NewHub(log), storeA, log)
	busB := events.NewPostgresBus(hubB, storeB, log)
	listenerB := &events.Listener{Connect: events.PGConnect(storeB.Connect), Hub: hubB, Log: log,
		MinBackoff: 20 * time.Millisecond, MaxBackoff: 100 * time.Millisecond}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = listenerB.Run(ctx) }()
	sub, _ := busB.Subscribe(ctx, app.ClaimChanged, "PL1")

	// Publish until the listener's session is up (LISTEN is asynchronous to us).
	publishUntilReceived := func(id string) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			busA.Publish(ctx, app.Event{Kind: app.ClaimChanged, ID: id, PlanID: "PL1"})
			select {
			case e := <-sub:
				if e.ID == id {
					return
				}
			case <-time.After(200 * time.Millisecond):
			}
		}
		t.Fatalf("event %s never crossed to the other replica", id)
	}
	publishUntilReceived("C1")

	// Kill the listener's session; it must reconnect and deliver again.
	admin, err := pgx.Connect(ctx, os.Getenv(pgtest.EnvDSN))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = admin.Close(context.Background()) }()
	var killed int
	if err := admin.QueryRow(ctx, "SELECT count(pg_terminate_backend(pid)) FROM pg_stat_activity WHERE application_name = 'demo-api-events'").Scan(&killed); err != nil || killed == 0 {
		t.Fatalf("terminate the listener session: killed %d, err %v", killed, err)
	}
	publishUntilReceived("C2")
}
