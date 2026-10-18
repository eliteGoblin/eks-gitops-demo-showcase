package postgres_test

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/eliteGoblin/eks-gitops-demo/internal/adapter/postgres"
)

// countingTracer counts the queries pgx reports to it.
type countingTracer struct{ started atomic.Int32 }

func (c *countingTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	c.started.Add(1)
	return ctx
}

func (c *countingTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// The composition root injects the telemetry tracer; the adapter only needs
// pgx's interface, so it stays free of OpenTelemetry.
func TestOpenAttachesTheQueryTracer(t *testing.T) {
	ctx := context.Background()
	tracer := &countingTracer{}
	pool, err := postgres.Open(ctx, testDSN(t), postgres.PoolOptions{MaxConns: 2, Tracer: tracer})
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	before := tracer.started.Load()
	if _, err := pool.Exec(ctx, "SELECT 1"); err != nil {
		t.Fatal(err)
	}
	if tracer.started.Load() == before {
		t.Fatal("the query was not reported to the tracer")
	}
}
