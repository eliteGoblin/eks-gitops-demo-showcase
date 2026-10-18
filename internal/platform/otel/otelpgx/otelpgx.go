// Package otelpgx instruments pgx: a client span per query inside a traced
// request, and the connection pool's gauges.
//
// Spans are named after the sqlc query ("GetPlan") and carry neither the SQL
// text nor its arguments, so no data leaves the process in a trace.
package otelpgx

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	kitotel "github.com/eliteGoblin/eks-gitops-demo/internal/platform/otel"
)

// Tracer is a pgx.QueryTracer. Set it on the pool's ConnConfig.Tracer.
type Tracer struct{ tracer trace.Tracer }

var _ pgx.QueryTracer = (*Tracer)(nil)

// NewTracer returns a tracer from the global provider.
func NewTracer() *Tracer { return &Tracer{tracer: otel.Tracer(kitotel.ScopeName)} }

// TraceQueryStart starts a span when the query runs inside a traced request.
// Queries outside one (the readiness ping, background work) are not traced:
// each would otherwise become a trace of its own.
func (t *Tracer) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if !trace.SpanContextFromContext(ctx).IsValid() {
		return ctx
	}
	name := queryName(data.SQL)
	attrs := []attribute.KeyValue{attribute.String("db.system.name", "postgresql"), attribute.String("db.operation.name", name)}
	if conn != nil {
		cfg := conn.Config()
		attrs = append(attrs, databaseAttributes(cfg.Database, cfg.Host)...)
	}
	ctx, _ = t.tracer.Start(ctx, name, trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(attrs...))
	return ctx
}

// databaseAttributes identify the database: so X-Ray's service map shows one
// "<database>@<host>" node instead of one node per query. X-Ray reads the
// pre-1.26 names db.name and db.connection_string (host only: no credentials).
func databaseAttributes(database, host string) []attribute.KeyValue {
	var attrs []attribute.KeyValue
	if database != "" {
		attrs = append(attrs, attribute.String("db.namespace", database), attribute.String("db.name", database))
	}
	if host != "" {
		attrs = append(attrs, attribute.String("server.address", host), attribute.String("db.connection_string", "postgresql://"+host))
	}
	return attrs
}

// TraceQueryEnd ends the span started by TraceQueryStart.
func (t *Tracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	span := trace.SpanFromContext(ctx)
	if data.Err != nil && !errors.Is(data.Err, pgx.ErrNoRows) {
		span.RecordError(data.Err)
		span.SetStatus(codes.Error, "query failed")
	}
	span.End()
}

var (
	sqlcName   = regexp.MustCompile(`^-- name: ([A-Za-z][A-Za-z0-9_]*) :`)
	sqlKeyword = regexp.MustCompile(`^[A-Za-z]{2,16}$`)
)

// queryName is the sqlc query name, else the statement's first keyword
// (BEGIN, COMMIT, SELECT…), else "query": always a short, bounded name.
func queryName(sql string) string {
	sql = strings.TrimSpace(sql)
	if m := sqlcName.FindStringSubmatch(sql); m != nil {
		return m[1]
	}
	if first, _, _ := strings.Cut(sql, " "); sqlKeyword.MatchString(first) {
		return strings.ToUpper(first)
	}
	return "query"
}

// RecordPoolStats reports the pool's connections as the db.pool.* gauges
// (acquired, idle, total and max), read at each metric export.
func RecordPoolStats(pool *pgxpool.Pool) error {
	return recordPoolStats(func() poolStat {
		s := pool.Stat()
		return poolStat{acquired: s.AcquiredConns(), idle: s.IdleConns(), total: s.TotalConns(), max: s.MaxConns()}
	})
}

type poolStat struct{ acquired, idle, total, max int32 }

func recordPoolStats(read func() poolStat) error {
	meter := kitotel.Meter()
	gauge := func(name, desc string) (metric.Int64ObservableGauge, error) {
		return meter.Int64ObservableGauge(name, metric.WithUnit("{connection}"), metric.WithDescription(desc))
	}
	acquired, err1 := gauge("db.pool.acquired_conns", "Connections currently in use.")
	idle, err2 := gauge("db.pool.idle_conns", "Open connections waiting to be used.")
	total, err3 := gauge("db.pool.total_conns", "Open connections, in use or idle.")
	maxConns, err4 := gauge("db.pool.max_conns", "The pool's connection limit.")
	if err := errors.Join(err1, err2, err3, err4); err != nil {
		return err
	}
	_, err := meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		s := read()
		o.ObserveInt64(acquired, int64(s.acquired))
		o.ObserveInt64(idle, int64(s.idle))
		o.ObserveInt64(total, int64(s.total))
		o.ObserveInt64(maxConns, int64(s.max))
		return nil
	}, acquired, idle, total, maxConns)
	return err
}
