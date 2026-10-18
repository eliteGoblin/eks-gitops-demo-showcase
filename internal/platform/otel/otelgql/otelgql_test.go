package otelgql

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/99designs/gqlgen/graphql"
	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/99designs/gqlgen/graphql/handler/transport"
	"github.com/vektah/gqlparser/v2"
	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/gqlerror"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/eliteGoblin/eks-gitops-demo/internal/platform"
)

var schema = gqlparser.MustLoadSchema(&ast.Source{Input: `
	type Query { me: String! participant(id: ID!): String plan(id: ID!): String claims: String }
	type Mutation { submitClaim: String approveClaim: String }
`})

// resolvers says what each root field returns: a value, or an error carrying
// an extensions.code.
type resolvers map[string]string

const errPrefix = "ERR:"

// newServer runs the extension in a gqlgen server whose executable schema
// resolves root fields through the operation's resolver middleware, as the
// generated code does.
func newServer(ext *Extension, res resolvers) http.Handler { return newServerOn(schema, ext, res) }

func newServerOn(s *ast.Schema, ext *Extension, res resolvers) http.Handler {
	srv := handler.New(&graphql.ExecutableSchemaMock{
		SchemaFunc:     func() *ast.Schema { return s },
		ComplexityFunc: func(context.Context, string, string, int, map[string]any) (int, bool) { return 1, true },
		ExecFunc: func(ctx context.Context) graphql.ResponseHandler {
			rc := graphql.GetOperationContext(ctx)
			done := false
			return func(ctx context.Context) *graphql.Response {
				if done {
					return nil
				}
				done = true
				root := "Query"
				if rc.Operation.Operation == ast.Mutation {
					root = "Mutation"
				}
				data := map[string]any{}
				for _, f := range graphql.CollectFields(rc, rc.Operation.SelectionSet, []string{root}) {
					fctx := graphql.WithFieldContext(ctx, &graphql.FieldContext{Object: root, Field: f, IsResolver: true})
					v, err := rc.ResolverMiddleware(fctx, func(context.Context) (any, error) {
						out := res[f.Name]
						if code, ok := strings.CutPrefix(out, errPrefix); ok {
							return nil, &gqlerror.Error{Message: "failed", Extensions: map[string]any{"code": code}}
						}
						return out, nil
					})
					if err != nil {
						graphql.AddError(fctx, err)
					}
					data[f.Alias] = v
				}
				b, _ := json.Marshal(data)
				return &graphql.Response{Data: b}
			}
		},
	})
	srv.AddTransport(transport.POST{})
	srv.Use(ext)
	return srv
}

type telemetry struct {
	spans  *tracetest.SpanRecorder
	reader *sdkmetric.ManualReader
}

// install sets fresh global providers; build the extension afterwards.
func install(t *testing.T) telemetry {
	t.Helper()
	spans := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()); _ = mp.Shutdown(context.Background()) })
	return telemetry{spans: spans, reader: reader}
}

func post(t *testing.T, h http.Handler, query string) {
	t.Helper()
	params := map[string]any{"query": query}
	if strings.Contains(query, "ClientChosenName") {
		params["operationName"] = "ClientChosenName" // must not reach the label
	}
	body, _ := json.Marshal(params)
	req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(httptest.NewRecorder(), req)
}

// operationPoint is one graphql.operations data point.
type operationPoint struct{ operation, outcome, code string }

func (tm telemetry) operations(t *testing.T) map[operationPoint]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := tm.reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[operationPoint]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "graphql.operations" {
				continue
			}
			for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
				op, _ := dp.Attributes.Value("operation")
				outcome, _ := dp.Attributes.Value("outcome")
				code, _ := dp.Attributes.Value("error_code")
				out[operationPoint{op.AsString(), outcome.AsString(), code.AsString()}] += dp.Value
			}
		}
	}
	return out
}

func (tm telemetry) metric(t *testing.T, name string) (metricdata.Metrics, bool) {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := tm.reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				return m, true
			}
		}
	}
	return metricdata.Metrics{}, false
}

func TestOperationLabelIsBoundedBySchemaRootFields(t *testing.T) {
	for _, tc := range []struct {
		name, query, want string
	}{
		{"single root field", `query { participant(id: "P1") }`, "query.participant"},
		{"alias ignored", `query { anything: participant(id: "P1") }`, "query.participant"},
		{"client operation name ignored", `query ClientChosenName { me }`, "query.me"},
		{"two root fields sorted", `{ plan(id: "PL1") me }`, "query.me+plan"},
		{"same field twice", `{ a: me b: me }`, "query.me"},
		{"more than two collapse", `{ me plan(id: "x") claims }`, "query.multiple"},
		{"fragments followed", `query { ...F } fragment F on Query { me }`, "query.me"},
		{"inline fragment followed", `query { ... on Query { claims } }`, "query.claims"},
		{"mutation", `mutation { submitClaim }`, "mutation.submitClaim"},
		{"introspection", `{ __typename }`, "query.__typename"},
		{"unknown field is other", `{ attackerControlled123 }`, "other"},
		{"parse failure is other", `{ me `, "other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tm := install(t)
			post(t, newServer(New(Options{}), resolvers{"me": "x", "participant": "x", "plan": "x", "claims": "x", "submitClaim": "x"}), tc.query)
			var ops []string
			for p := range tm.operations(t) {
				ops = append(ops, p.operation)
			}
			if !slices.Equal(ops, []string{tc.want}) {
				t.Errorf("operation labels = %v, want [%s]", ops, tc.want)
			}
		})
	}
}

func TestOutcomeSeparatesServerFromClientErrors(t *testing.T) {
	for _, tc := range []struct {
		name  string
		res   resolvers
		query string
		want  operationPoint
	}{
		{"success", resolvers{"me": "x"}, `{ me }`, operationPoint{"query.me", "ok", "none"}},
		{"forbidden is a client error", resolvers{"submitClaim": errPrefix + platform.CodeForbidden}, `mutation { submitClaim }`,
			operationPoint{"mutation.submitClaim", "client_error", platform.CodeForbidden}},
		{"bad input is a client error", resolvers{"submitClaim": errPrefix + platform.CodeBadUserInput}, `mutation { submitClaim }`,
			operationPoint{"mutation.submitClaim", "client_error", platform.CodeBadUserInput}},
		{"internal is a server error", resolvers{"me": errPrefix + platform.CodeInternal}, `{ me }`,
			operationPoint{"query.me", "error", platform.CodeInternal}},
		{"unavailable is a server error", resolvers{"me": errPrefix + platform.CodeUnavailable}, `{ me }`,
			operationPoint{"query.me", "error", platform.CodeUnavailable}},
		{"a request the client cancelled is a client error", resolvers{"me": errPrefix + platform.CodeCancelled}, `{ me }`,
			operationPoint{"query.me", "client_error", platform.CodeCancelled}},
		{"a server error outranks a client error", resolvers{"me": errPrefix + platform.CodeForbidden, "plan": errPrefix + platform.CodeInternal},
			`{ me plan(id: "x") }`, operationPoint{"query.me+plan", "error", platform.CodeInternal}},
		{"unknown codes are bounded", resolvers{"me": errPrefix + "SOMETHING_NEW"}, `{ me }`,
			operationPoint{"query.me", "error", "OTHER"}},
		{"extra client codes", resolvers{"me": errPrefix + "DEPTH_LIMIT_EXCEEDED"}, `{ me }`,
			operationPoint{"query.me", "client_error", "DEPTH_LIMIT_EXCEEDED"}},
		{"validation failure", resolvers{}, `{ nope }`, operationPoint{"other", "client_error", "GRAPHQL_VALIDATION_FAILED"}},
		{"parse failure", resolvers{}, `{ me `, operationPoint{"other", "client_error", "GRAPHQL_PARSE_FAILED"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tm := install(t)
			post(t, newServer(New(Options{ClientErrorCodes: []string{"DEPTH_LIMIT_EXCEEDED"}}), tc.res), tc.query)
			got := tm.operations(t)
			if len(got) != 1 || got[tc.want] != 1 {
				t.Errorf("graphql.operations = %v, want {%v: 1}", got, tc.want)
			}
		})
	}
}

func TestDurationUsesTheOperationLabelOnly(t *testing.T) {
	tm := install(t)
	post(t, newServer(New(Options{}), resolvers{"me": "x"}), `{ me }`)
	m, ok := tm.metric(t, "graphql.operation.duration")
	if !ok {
		t.Fatal("graphql.operation.duration not recorded")
	}
	if m.Unit != "s" {
		t.Errorf("unit = %q, want s (seconds-based buckets come from the kit's view)", m.Unit)
	}
	dp := m.Data.(metricdata.Histogram[float64]).DataPoints[0]
	if dp.Attributes.Len() != 1 {
		t.Errorf("attributes = %v, want only operation", dp.Attributes)
	}
	if op, _ := dp.Attributes.Value("operation"); op.AsString() != "query.me" || dp.Count != 1 {
		t.Errorf("data point = %v count %d", dp.Attributes, dp.Count)
	}
}

func TestAuthFailuresCountForbiddenAs403(t *testing.T) {
	tm := install(t)
	post(t, newServer(New(Options{}), resolvers{"submitClaim": errPrefix + platform.CodeForbidden}), `mutation { submitClaim }`)
	m, ok := tm.metric(t, "auth.failures")
	if !ok {
		t.Fatal("auth.failures not recorded")
	}
	dp := m.Data.(metricdata.Sum[int64]).DataPoints[0]
	if code, _ := dp.Attributes.Value("code"); code.AsString() != "403" || dp.Value != 1 {
		t.Errorf("auth.failures = %v %d, want code=403 1", dp.Attributes, dp.Value)
	}
}

func TestSpansCoverTheOperationAndRootResolversOnly(t *testing.T) {
	tm := install(t)
	tracer := otel.Tracer("test")
	h := newServer(New(Options{}), resolvers{"me": "x", "plan": errPrefix + platform.CodeInternal})
	parentCtx, parent := tracer.Start(context.Background(), "POST /graphql")
	body := `{"query":"{ me plan(id: \"x\") }"}`
	req := httptest.NewRequestWithContext(parentCtx, http.MethodPost, "/graphql", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(httptest.NewRecorder(), req)
	parent.End()

	byName := map[string]sdktrace.ReadOnlySpan{}
	for _, s := range tm.spans.Ended() {
		byName[s.Name()] = s
	}
	op, ok := byName["query.me+plan"]
	if !ok {
		t.Fatalf("no operation span; spans: %v", keys(byName))
	}
	if op.Parent().SpanID() != parent.SpanContext().SpanID() {
		t.Error("the operation span must be a child of the HTTP server span")
	}
	if op.Status().Code.String() != "Error" {
		t.Errorf("operation span status = %v, want Error for a server-side error", op.Status())
	}
	for _, name := range []string{"Query.me", "Query.plan"} {
		s, ok := byName[name]
		if !ok {
			t.Fatalf("no resolver span %s; spans: %v", name, keys(byName))
		}
		if s.Parent().SpanID() != op.SpanContext().SpanID() {
			t.Errorf("%s must be a child of the operation span", name)
		}
	}
	if got := len(tm.spans.Ended()); got != 4 {
		t.Errorf("%d spans, want 4 (http, operation, two root resolvers): %v", got, keys(byName))
	}
	if s := byName["Query.plan"]; s.Status().Code.String() == "Error" || len(s.Events()) == 0 {
		t.Error("a resolver error is recorded as an event, without marking the span failed (it may be the caller's mistake)")
	}
}

func keys(m map[string]sdktrace.ReadOnlySpan) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

var withDeprecations = gqlparser.MustLoadSchema(&ast.Source{Input: `
	type Query { me: String! old: String @deprecated(reason: "Use me") person: Person }
	type Person { name: String! nick: String @deprecated(reason: "Use name") }
`})

func TestDeprecatedFieldUsageCountsOperationsPerField(t *testing.T) {
	tm := install(t)
	h := newServerOn(withDeprecations, New(Options{}), resolvers{"me": "x", "old": "x", "person": "x"})
	post(t, h, `{ me person { name } }`)
	post(t, h, `{ old again: old person { nick ...N } } fragment N on Person { nick name }`)
	post(t, h, `{ person { ... on Person { nick } } }`)
	post(t, h, `{ old unknown }`) // invalid: rejected before it runs, not counted

	m, ok := tm.metric(t, "graphql.deprecated_field_usage")
	if !ok {
		t.Fatal("graphql.deprecated_field_usage not recorded")
	}
	got := map[string]int64{}
	for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
		field, _ := dp.Attributes.Value("field")
		got[field.AsString()] += dp.Value
	}
	want := map[string]int64{"Query.old": 1, "Person.nick": 2}
	if len(got) != len(want) || got["Query.old"] != 1 || got["Person.nick"] != 2 {
		t.Errorf("deprecated_field_usage = %v, want %v (once per operation, aliases and fragments included)", got, want)
	}
}

// Every deprecated field has a series from startup, at zero, so "unused for
// 30 days" can be measured: a series that never existed reads as no data,
// not as zero, and increase() over a new series starts from its first sample.
func TestDeprecatedFieldsAreCountedFromZero(t *testing.T) {
	tm := install(t)
	_ = newServerOn(withDeprecations, New(Options{}), resolvers{})
	m, ok := tm.metric(t, "graphql.deprecated_field_usage")
	if !ok {
		t.Fatal("graphql.deprecated_field_usage has no series before the first use")
	}
	got := map[string]int64{}
	for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
		field, _ := dp.Attributes.Value("field")
		got[field.AsString()] = dp.Value
	}
	if len(got) != 2 || got["Query.old"] != 0 || got["Person.nick"] != 0 {
		t.Fatalf("deprecated_field_usage at startup = %v, want Query.old and Person.nick at 0", got)
	}
}
