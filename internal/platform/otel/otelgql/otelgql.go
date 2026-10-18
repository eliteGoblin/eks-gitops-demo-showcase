// Package otelgql instruments a gqlgen server: a span per operation, a child
// span per root resolver, and the graphql.operations counter and
// graphql.operation.duration histogram.
//
// Every label is bounded. The operation label is built from the schema's root
// fields ("query.participant", "mutation.submitClaim", "query.me+plan"), never
// from the client's operationName, and is "other" for anything else (a
// document that does not parse or validate); error codes outside the known
// set become "OTHER". graphql.deprecated_field_usage counts the operations
// that select a deprecated field, labelled "Type.field" (a schema name, so
// bounded too): the signal for when a deprecated field can be removed.
package otelgql

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/99designs/gqlgen/graphql"
	"github.com/99designs/gqlgen/graphql/errcode"
	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/gqlerror"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/eliteGoblin/eks-gitops-demo/internal/platform"
	kitotel "github.com/eliteGoblin/eks-gitops-demo/internal/platform/otel"
)

// Outcome and error_code label values.
const (
	OutcomeOK          = "ok"
	OutcomeClientError = "client_error" // the caller's mistake or a denial: bad input, forbidden, invalid query
	OutcomeError       = "error"        // the server failed: what the error-rate alert counts

	codeNone  = "none"
	codeOther = "OTHER"

	otherOperation = "other"
	maxRootFields  = 2
)

// complexityLimitExceeded is the code gqlgen's complexity extension sets (unexported there).
const complexityLimitExceeded = "COMPLEXITY_LIMIT_EXCEEDED"

var (
	defaultClientCodes = []string{
		platform.CodeUnauthenticated, platform.CodeForbidden, platform.CodeBadUserInput,
		platform.CodeRateLimited, platform.CodeNotFound, platform.CodeConflict, platform.CodeCancelled,
		errcode.ParseFailed, errcode.ValidationFailed, complexityLimitExceeded,
	}
	serverCodes = []string{platform.CodeInternal, platform.CodeUnavailable}
)

// Options configure the extension.
type Options struct {
	// ClientErrorCodes are extensions.code values the service adds to the
	// kit's client-error codes (e.g. its own depth-limit code).
	ClientErrorCodes []string
}

// Extension is the gqlgen handler extension. Add it with Server.Use.
type Extension struct {
	tracer       trace.Tracer
	operations   metric.Int64Counter
	duration     metric.Float64Histogram
	authFailures metric.Int64Counter
	deprecated   metric.Int64Counter
	clientCodes  []string
	schema       *ast.Schema
}

var _ interface {
	graphql.HandlerExtension
	graphql.OperationInterceptor
	graphql.ResponseInterceptor
	graphql.FieldInterceptor
} = (*Extension)(nil)

// New builds the extension from the global providers (call it after the
// kit's Setup).
func New(opts Options) *Extension {
	meter := kitotel.Meter()
	operations, err := meter.Int64Counter("graphql.operations",
		metric.WithUnit("{operation}"),
		metric.WithDescription("GraphQL operations by root fields, outcome and error code."))
	if err != nil {
		otel.Handle(err)
	}
	duration, err := meter.Float64Histogram("graphql.operation.duration",
		metric.WithUnit("s"),
		metric.WithDescription("Time from receiving a GraphQL operation to its response."))
	if err != nil {
		otel.Handle(err)
	}
	deprecated, err := meter.Int64Counter("graphql.deprecated_field_usage",
		metric.WithUnit("{operation}"),
		metric.WithDescription("GraphQL operations that selected a deprecated field, by field (Type.field)."))
	if err != nil {
		otel.Handle(err)
	}
	return &Extension{
		tracer:       otel.Tracer(kitotel.ScopeName),
		operations:   operations,
		duration:     duration,
		authFailures: kitotel.AuthFailures(meter),
		deprecated:   deprecated,
		clientCodes:  slices.Concat(defaultClientCodes, opts.ClientErrorCodes),
	}
}

// ExtensionName implements graphql.HandlerExtension.
func (e *Extension) ExtensionName() string { return "OpenTelemetry" }

// Validate implements graphql.HandlerExtension; it keeps the schema to check
// root field names against and starts the deprecated-field series.
func (e *Extension) Validate(schema graphql.ExecutableSchema) error {
	e.schema = schema.Schema()
	for _, field := range deprecatedSchemaFields(e.schema) {
		// Zero from startup: a field nobody uses then reads as an increase of
		// 0 over any window, rather than as no data, and its first real use
		// is counted by increase() (which takes a new series' first sample as
		// its baseline).
		e.deprecated.Add(context.Background(), 0, metric.WithAttributes(attribute.String("field", field)))
	}
	return nil
}

// deprecatedSchemaFields returns, sorted, every deprecated field of the
// schema's object and interface types as "Type.field".
func deprecatedSchemaFields(s *ast.Schema) []string {
	var fields []string
	for name, def := range s.Types {
		if strings.HasPrefix(name, "__") || (def.Kind != ast.Object && def.Kind != ast.Interface) {
			continue
		}
		for _, f := range def.Fields {
			if f.Directives.ForName("deprecated") != nil {
				fields = append(fields, name+"."+f.Name)
			}
		}
	}
	slices.Sort(fields)
	return fields
}

// InterceptOperation counts, once per operation (a subscription counts once,
// not per event), each deprecated field the validated operation selects.
func (e *Extension) InterceptOperation(ctx context.Context, next graphql.OperationHandler) graphql.ResponseHandler {
	if graphql.HasOperationContext(ctx) {
		rc := graphql.GetOperationContext(ctx)
		if rc.Operation != nil {
			for _, field := range deprecatedFields(rc.Doc, rc.Operation.SelectionSet) {
				e.deprecated.Add(ctx, 1, metric.WithAttributes(attribute.String("field", field)))
			}
		}
	}
	return next(ctx)
}

// deprecatedFields returns, sorted and distinct, the deprecated fields a
// validated selection set selects as "Type.field", following fragments.
func deprecatedFields(doc *ast.QueryDocument, set ast.SelectionSet) []string {
	seen := map[string]bool{}
	visited := map[string]bool{}
	var walk func(ast.SelectionSet)
	walk = func(set ast.SelectionSet) {
		for _, sel := range set {
			switch s := sel.(type) {
			case *ast.Field:
				if s.Definition != nil && s.ObjectDefinition != nil && s.Definition.Directives.ForName("deprecated") != nil {
					seen[s.ObjectDefinition.Name+"."+s.Name] = true
				}
				walk(s.SelectionSet)
			case *ast.InlineFragment:
				walk(s.SelectionSet)
			case *ast.FragmentSpread:
				if visited[s.Name] {
					continue
				}
				visited[s.Name] = true
				if def := fragmentOf(doc, s); def != nil {
					walk(def.SelectionSet)
				}
			}
		}
	}
	walk(set)
	fields := make([]string, 0, len(seen))
	for f := range seen {
		fields = append(fields, f)
	}
	slices.Sort(fields)
	return fields
}

func fragmentOf(doc *ast.QueryDocument, s *ast.FragmentSpread) *ast.FragmentDefinition {
	if s.Definition != nil {
		return s.Definition
	}
	if doc != nil {
		return doc.Fragments.ForName(s.Name)
	}
	return nil
}

// InterceptResponse wraps every response: the operation span, the metrics,
// and auth.failures for FORBIDDEN and UNAUTHENTICATED errors. It also sees
// operations rejected before execution (parse, validation, limits).
func (e *Extension) InterceptResponse(ctx context.Context, next graphql.ResponseHandler) *graphql.Response {
	op := e.operationOf(ctx)
	start := operationStart(ctx)
	ctx, span := e.tracer.Start(ctx, op.label, trace.WithAttributes(attribute.String("graphql.operation.type", op.kind)))
	defer span.End()

	resp := next(ctx)
	if resp == nil { // the end of a subscription stream
		return nil
	}
	outcome, code := e.classify(resp.Errors)
	span.SetAttributes(attribute.String("graphql.outcome", outcome), attribute.String("graphql.error_code", code))
	if outcome == OutcomeError {
		span.SetStatus(codes.Error, code)
	}
	e.operations.Add(ctx, 1, metric.WithAttributes(
		attribute.String("operation", op.label), attribute.String("outcome", outcome), attribute.String("error_code", code)))
	if op.kind != string(ast.Subscription) { // a subscription event's age is not a latency
		e.duration.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(attribute.String("operation", op.label)))
	}
	e.countAuthFailures(ctx, resp.Errors)
	return resp
}

// InterceptField adds a span around each root resolver. Nested fields are
// not traced: their database calls appear as children of the operation span.
func (e *Extension) InterceptField(ctx context.Context, next graphql.Resolver) (any, error) {
	fc := graphql.GetFieldContext(ctx)
	if fc == nil || !e.isRootType(fc.Object) {
		return next(ctx)
	}
	ctx, span := e.tracer.Start(ctx, fc.Object+"."+fc.Field.Name)
	defer span.End()
	res, err := next(ctx)
	if err != nil {
		span.RecordError(err) // not a failed span: it may be the caller's mistake
	}
	return res, err
}

// classify returns the outcome and error_code labels: the first server-side
// error decides, otherwise the first client error.
func (e *Extension) classify(errs gqlerror.List) (outcome, code string) {
	if len(errs) == 0 {
		return OutcomeOK, codeNone
	}
	firstClient := ""
	for _, err := range errs {
		c := errorCode(err)
		if slices.Contains(e.clientCodes, c) {
			if firstClient == "" {
				firstClient = c
			}
			continue
		}
		if !slices.Contains(serverCodes, c) {
			c = codeOther
		}
		return OutcomeError, c
	}
	return OutcomeClientError, firstClient
}

func (e *Extension) countAuthFailures(ctx context.Context, errs gqlerror.List) {
	counted := map[int]bool{}
	for _, err := range errs {
		status := 0
		switch errorCode(err) {
		case platform.CodeUnauthenticated:
			status = 401
		case platform.CodeForbidden:
			status = 403
		}
		if status != 0 && !counted[status] {
			counted[status] = true
			e.authFailures.Add(ctx, 1, metric.WithAttributes(kitotel.AuthFailureCode(status)))
		}
	}
}

func errorCode(err *gqlerror.Error) string {
	code, _ := err.Extensions["code"].(string)
	return code
}

func operationStart(ctx context.Context) time.Time {
	if graphql.HasOperationContext(ctx) {
		if t := graphql.GetOperationContext(ctx).Stats.OperationStart; !t.IsZero() {
			return t
		}
	}
	return time.Now()
}

// operation is the bounded identity of a GraphQL operation.
type operation struct {
	label string // e.g. "query.participant", "query.me+plan", "query.multiple" or "other"
	kind  string // "query", "mutation", "subscription"; empty for "other"
}

func (e *Extension) operationOf(ctx context.Context) operation {
	if e.schema == nil || !graphql.HasOperationContext(ctx) {
		return operation{label: otherOperation}
	}
	rc := graphql.GetOperationContext(ctx)
	if rc.Operation == nil {
		return operation{label: otherOperation}
	}
	root := e.rootType(rc.Operation.Operation)
	if root == nil {
		return operation{label: otherOperation}
	}
	names, ok := rootFieldNames(rc.Doc, rc.Operation.SelectionSet, root)
	if !ok || len(names) == 0 {
		return operation{label: otherOperation}
	}
	kind := string(rc.Operation.Operation)
	if len(names) > maxRootFields {
		return operation{label: kind + ".multiple", kind: kind}
	}
	return operation{label: kind + "." + strings.Join(names, "+"), kind: kind}
}

func (e *Extension) rootType(op ast.Operation) *ast.Definition {
	switch op {
	case ast.Query:
		return e.schema.Query
	case ast.Mutation:
		return e.schema.Mutation
	case ast.Subscription:
		return e.schema.Subscription
	}
	return nil
}

func (e *Extension) isRootType(name string) bool {
	if e.schema == nil {
		return false
	}
	for _, root := range []*ast.Definition{e.schema.Query, e.schema.Mutation, e.schema.Subscription} {
		if root != nil && root.Name == name {
			return true
		}
	}
	return false
}

// rootFieldNames returns the sorted, distinct schema fields selected at the
// root, following fragments. ok is false when a selected name is not a field
// of root: the document failed validation, so its names are arbitrary text.
func rootFieldNames(doc *ast.QueryDocument, set ast.SelectionSet, root *ast.Definition) (names []string, ok bool) {
	seen := map[string]bool{}
	visited := map[string]bool{}
	var walk func(ast.SelectionSet) bool
	walk = func(set ast.SelectionSet) bool {
		for _, sel := range set {
			switch s := sel.(type) {
			case *ast.Field:
				if !isRootField(root, s.Name) {
					return false
				}
				seen[s.Name] = true
			case *ast.InlineFragment:
				if !walk(s.SelectionSet) {
					return false
				}
			case *ast.FragmentSpread:
				if visited[s.Name] {
					continue
				}
				visited[s.Name] = true
				var def *ast.FragmentDefinition
				if doc != nil {
					def = doc.Fragments.ForName(s.Name)
				}
				if def == nil || !walk(def.SelectionSet) {
					return false
				}
			}
		}
		return true
	}
	if !walk(set) {
		return nil, false
	}
	for name := range seen {
		names = append(names, name)
	}
	slices.Sort(names)
	return names, true
}

func isRootField(root *ast.Definition, name string) bool {
	switch name {
	case "__typename", "__schema", "__type":
		return true
	}
	return root.Fields.ForName(name) != nil
}
