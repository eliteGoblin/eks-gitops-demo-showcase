package otelpgx

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func TestQueryName(t *testing.T) {
	for sql, want := range map[string]string{
		"-- name: GetPlan :one\nSELECT id FROM plans WHERE id = $1": "GetPlan",
		"-- name: InsertClaim :exec\nINSERT INTO claims …":          "InsertClaim",
		"begin":                    "BEGIN",
		"  select 1":               "SELECT",
		"-- ping":                  "query",
		"":                         "query",
		"-- name: Bad Name! :one":  "query",
		"WITH x AS (SELECT 1) ...": "WITH",
		"select/*comment*/1":       "query",
	} {
		if got := queryName(sql); got != want {
			t.Errorf("queryName(%q) = %q, want %q", sql, got, want)
		}
	}
}

func TestDatabaseAttributesNameOneServiceNode(t *testing.T) {
	attrs := attribute.NewSet(databaseAttributes("demoapi", "db.internal")...)
	for key, want := range map[attribute.Key]string{
		"db.namespace":   "demoapi",
		"server.address": "db.internal",
		// X-Ray names a database node "<db.name>@<host of db.connection_string>",
		// reading the pre-1.26 attribute names.
		"db.name":              "demoapi",
		"db.connection_string": "postgresql://db.internal",
	} {
		if v, _ := attrs.Value(key); v.AsString() != want {
			t.Errorf("%s = %q, want %q", key, v.AsString(), want)
		}
	}
	if got := databaseAttributes("", ""); len(got) != 0 {
		t.Errorf("no connection details, no attributes; got %v", got)
	}
}

func installTracer(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	spans := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	return spans
}

func TestTracerRecordsAQuerySpanInsideARequest(t *testing.T) {
	spans := installTracer(t)
	tr := NewTracer()
	ctx, parent := otel.Tracer("test").Start(context.Background(), "POST /graphql")

	qctx := tr.TraceQueryStart(ctx, nil, pgx.TraceQueryStartData{SQL: "-- name: GetPlan :one\nSELECT secret FROM plans WHERE id = $1", Args: []any{"PL1"}})
	tr.TraceQueryEnd(qctx, nil, pgx.TraceQueryEndData{})
	parent.End()

	var q sdktrace.ReadOnlySpan
	for _, s := range spans.Ended() {
		if s.Name() == "GetPlan" {
			q = s
		}
	}
	if q == nil {
		t.Fatalf("no query span among %d spans", len(spans.Ended()))
	}
	if q.Parent().SpanID() != parent.SpanContext().SpanID() || q.SpanKind() != trace.SpanKindClient {
		t.Error("the query span must be a client span under the request span")
	}
	attrs := attribute.NewSet(q.Attributes()...)
	if v, _ := attrs.Value("db.system.name"); v.AsString() != "postgresql" {
		t.Errorf("db.system.name = %v", v)
	}
	if v, _ := attrs.Value("db.operation.name"); v.AsString() != "GetPlan" {
		t.Errorf("db.operation.name = %v", v)
	}
	for _, kv := range q.Attributes() {
		if v := kv.Value.String(); v == "PL1" || len(v) > 40 {
			t.Errorf("SQL text or arguments leaked into %s=%q", kv.Key, v)
		}
	}
}

func TestTracerStartsNoTraceOnItsOwn(t *testing.T) {
	spans := installTracer(t)
	tr := NewTracer()
	ctx := context.Background() // e.g. the readiness probe's ping
	qctx := tr.TraceQueryStart(ctx, nil, pgx.TraceQueryStartData{SQL: "-- ping"})
	tr.TraceQueryEnd(qctx, nil, pgx.TraceQueryEndData{})
	if n := len(spans.Ended()); n != 0 {
		t.Errorf("%d spans outside a traced request, want 0", n)
	}
}

func TestTracerMarksFailedQueries(t *testing.T) {
	spans := installTracer(t)
	tr := NewTracer()
	for _, tc := range []struct {
		err    error
		failed bool
	}{
		{errors.New("lock timeout"), true},
		{pgx.ErrNoRows, false}, // not finding a row is an answer, not a failure
	} {
		ctx, parent := otel.Tracer("test").Start(context.Background(), "req")
		qctx := tr.TraceQueryStart(ctx, nil, pgx.TraceQueryStartData{SQL: "-- name: LockPlan :one\nSELECT"})
		tr.TraceQueryEnd(qctx, nil, pgx.TraceQueryEndData{Err: tc.err})
		parent.End()
		last := spans.Ended()[len(spans.Ended())-2] // the query ended before its parent
		if failed := last.Status().Code.String() == "Error"; failed != tc.failed {
			t.Errorf("err %v: span failed = %v, want %v", tc.err, failed, tc.failed)
		}
	}
}

func TestPoolGauges(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	otel.SetMeterProvider(mp)
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	if err := recordPoolStats(func() poolStat { return poolStat{acquired: 3, idle: 2, total: 5, max: 8} }); err != nil {
		t.Fatal(err)
	}
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Unit != "{connection}" {
				t.Errorf("%s unit = %q, want {connection} (no Prometheus unit suffix)", m.Name, m.Unit)
			}
			got[m.Name] = m.Data.(metricdata.Gauge[int64]).DataPoints[0].Value
		}
	}
	want := map[string]int64{"db.pool.acquired_conns": 3, "db.pool.idle_conns": 2, "db.pool.total_conns": 5, "db.pool.max_conns": 8}
	for name, v := range want {
		if got[name] != v {
			t.Errorf("%s = %d, want %d (all: %v)", name, got[name], v, got)
		}
	}
}
