package graphql

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	gql "github.com/99designs/gqlgen/graphql"
	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/99designs/gqlgen/graphql/handler/extension"
	"github.com/99designs/gqlgen/graphql/handler/lru"
	"github.com/99designs/gqlgen/graphql/handler/transport"
	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/gqlerror"

	"github.com/eliteGoblin/eks-gitops-demo/internal/adapter/graphql/generated"
	"github.com/eliteGoblin/eks-gitops-demo/internal/adapter/graphql/model"
	"github.com/eliteGoblin/eks-gitops-demo/internal/app"
	"github.com/eliteGoblin/eks-gitops-demo/internal/domain"
	"github.com/eliteGoblin/eks-gitops-demo/internal/platform"
)

// Options configure the GraphQL handler.
type Options struct {
	Introspection    bool // allow __schema/__type queries (development and the playground)
	DepthLimit       int  // maximum field nesting of an operation
	ComplexityLimit  int  // maximum cost; paginated fields cost per requested item
	MaxDocumentBytes int  // largest document accepted; 0 means DefaultMaxDocumentBytes
	Log              *slog.Logger
	Telemetry        gql.HandlerExtension // OpenTelemetry instrumentation; nil for none
	Persisted        gql.HandlerExtension // APQ or TrustedDocuments; nil runs any document sent in full
	Subscriptions    *SubscriptionOptions // live updates over WebSocket; nil serves POST only
}

// NewHandler builds the GraphQL HTTP handler: queries and mutations over POST,
// subscriptions over WebSocket (when enabled), query cache, depth and
// complexity limits, @auth directive and the error presenter.
func NewHandler(svc *app.Services, opt Options) *handler.Server {
	cfg := generated.Config{Resolvers: &Resolver{Svc: svc}}
	cfg.Directives.Auth = authDirective(svc.RoleGate)
	claimsCost := func(child int, first *int, _ *string, _ *domain.ClaimStatus, _ *model.ClaimFilter, _ *model.ClaimOrder) int {
		return connectionCost(child, first)
	}
	cfg.Complexity.Query.Claims = claimsCost
	cfg.Complexity.Plan.Claims = claimsCost
	cfg.Complexity.Query.Node = func(child int, _ string) int { return nodeLookupCost + child }
	cfg.Complexity.Query.Nodes = func(child int, ids []string) int { return nodeLookupCost + len(ids)*(child+1) }
	cfg.Complexity.Query.Search = func(child int, _ string, first *int) int { return searchCost + searchSize(first)*child }
	srv := handler.New(generated.NewExecutableSchema(cfg))
	srv.AddTransport(transport.POST{})
	if opt.Subscriptions != nil {
		srv.AddTransport(websocketTransport(*opt.Subscriptions, opt.Log))
	}
	if opt.Telemetry != nil {
		srv.Use(opt.Telemetry) // first, so its timing covers the other extensions
	}
	srv.Use(documentSizeLimit{max: documentLimit(opt.MaxDocumentBytes)}) // before anything parses or caches the document
	if opt.Persisted != nil {
		srv.Use(opt.Persisted) // resolves hashes before the document is parsed
	}
	srv.AroundOperations(operationsByTransport)
	// Loaders live for one response: a subscription event re-reads, never
	// serving records cached from an earlier event.
	srv.AroundResponses(func(ctx context.Context, next gql.ResponseHandler) *gql.Response {
		return next(withLoaders(ctx, newLoaders(svc)))
	})
	srv.SetQueryCache(lru.New[*ast.QueryDocument](1000))
	srv.Use(depthLimit{max: opt.DepthLimit}) // before complexity, so a deep query reports depth
	srv.Use(extension.FixedComplexityLimit(opt.ComplexityLimit))
	if opt.Introspection {
		srv.Use(extension.Introspection{})
	}
	srv.SetErrorPresenter(presenter(opt.Log))
	srv.SetRecoverFunc(func(ctx context.Context, p any) error {
		opt.Log.ErrorContext(ctx, "panic in resolver", "panic", p)
		return errors.New("internal error")
	})
	return srv
}

// documentLimit is the configured document size limit, or the default.
func documentLimit(configured int) int {
	if configured > 0 {
		return configured
	}
	return DefaultMaxDocumentBytes
}

// authDirective is the coarse role gate; relationship checks live in the app
// layer. The app's RoleGate decides, and audits a refusal as the action the
// field attempts on the record its arguments name: the use case behind a
// refused field never runs, so only the gate can put the attempt on record.
func authDirective(gate app.RoleGate) func(context.Context, any, gql.Resolver, []model.Role) (any, error) {
	return func(ctx context.Context, _ any, next gql.Resolver, requires []model.Role) (any, error) {
		roles := make([]app.Role, len(requires))
		for i, r := range requires {
			roles[i] = app.Role(strings.ToLower(string(r)))
		}
		action, res := attempt(gql.GetFieldContext(ctx))
		if err := gate.Require(ctx, action, res, roles...); err != nil {
			return nil, err
		}
		return next(ctx)
	}
}

// attempt names the use-case action a field behind @auth performs and the
// record its arguments name, so a refusal reads in the audit trail like that
// use case's own decision. Only the arguments are used: nothing is looked up
// for a caller who is about to be refused.
func attempt(fc *gql.FieldContext) (app.Action, app.Resource) {
	field := fc.Object + "." + fc.Field.Name
	switch field {
	case "Mutation.submitClaim":
		in, _ := fc.Args["input"].(model.SubmitClaimInput)
		return app.ActionSubmitClaim, app.Resource{PlanID: in.PlanID, Category: in.Category}
	case "Mutation.approveClaim":
		id, _ := fc.Args["id"].(string)
		return app.ActionApproveClaim, app.Resource{ClaimID: id}
	case "Mutation.raiseSupportRequest":
		in, _ := fc.Args["input"].(model.RaiseSupportRequestInput)
		return app.ActionRaiseSupportRequest, app.Resource{PlanID: in.PlanID, Category: in.Category}
	case "Mutation.decideSupportRequest":
		in, _ := fc.Args["input"].(model.DecideSupportRequestInput)
		return app.ActionDecideSupportRequest, app.Resource{SupportRequestID: in.ID}
	case "Query.pendingSupportRequests":
		return app.ActionReadSupportQueue, app.Resource{}
	default:
		return app.Action(field), app.Resource{} // a new gated field: TestEveryGatedFieldNamesItsAction fails
	}
}

// presenter maps app and domain errors to extensions.code and hides internals.
func presenter(log *slog.Logger) gql.ErrorPresenterFunc {
	return func(ctx context.Context, err error) *gqlerror.Error {
		gqlErr := gql.DefaultErrorPresenter(ctx, err)
		if gqlErr.Extensions == nil {
			gqlErr.Extensions = map[string]any{}
		}
		if _, hasCode := gqlErr.Extensions["code"]; hasCode {
			return gqlErr // already classified by gqlgen (parser, validation, complexity)
		}
		if isMalformedRequest(err) {
			// The transport could not read the body. Its text echoes the body,
			// which may carry personal data, so it stays out of the response
			// and the log.
			log.InfoContext(ctx, "malformed request")
			gqlErr.Extensions["code"] = platform.CodeBadUserInput
			gqlErr.Message = "malformed request"
			return gqlErr
		}
		switch {
		case errors.Is(err, app.ErrUnauthenticated):
			gqlErr.Extensions["code"] = platform.CodeUnauthenticated
		case errors.Is(err, app.ErrForbidden):
			gqlErr.Extensions["code"] = platform.CodeForbidden
			gqlErr.Message = app.ErrForbidden.Error() // never explain why access was denied
		case errors.Is(err, domain.ErrInvalidInput), isBusiness(err):
			var refused interface{ SQLState() string }
			if errors.As(err, &refused) {
				log.WarnContext(ctx, "storage refused a request value", "sqlstate", refused.SQLState())
			}
			gqlErr.Extensions["code"] = platform.CodeBadUserInput
		// Before the server-side cases: a client that went away can make the
		// database report its statement as cut short (57014), which would
		// otherwise read as storage being busy.
		case errors.Is(err, context.Canceled), errors.Is(ctx.Err(), context.Canceled):
			gqlErr.Extensions["code"] = platform.CodeCancelled
			gqlErr.Message = "request cancelled"
		case errors.Is(err, app.ErrUnavailable):
			log.WarnContext(ctx, "storage busy", "err", err)
			gqlErr.Extensions["code"] = platform.CodeUnavailable
			gqlErr.Message = "temporarily unavailable, try again"
		case errors.Is(err, context.DeadlineExceeded):
			log.WarnContext(ctx, "request timed out", "err", err)
			gqlErr.Extensions["code"] = platform.CodeUnavailable
			gqlErr.Message = "request timed out"
		default:
			log.ErrorContext(ctx, "internal error", "err", err)
			gqlErr.Extensions["code"] = platform.CodeInternal
			gqlErr.Message = "internal error"
		}
		return gqlErr
	}
}

// isMalformedRequest reports whether err is gqlgen refusing a request it
// could not read: an error it built itself (wrapping no Go error), not raised
// on a field (a resolver's error always has a path) and without a code.
// Errors that wrap a Go error, such as a recovered panic, stay server errors.
func isMalformedRequest(err error) bool {
	var gqlErr *gqlerror.Error
	return errors.As(err, &gqlErr) && gqlErr.Path == nil && errors.Unwrap(gqlErr) == nil
}

func isBusiness(err error) bool {
	_, ok := businessError(err)
	return ok
}

// businessError maps a domain rule failure to its union member. Each union
// accepts only some members; the resolver checks with a type assertion.
func businessError(err error) (any, bool) {
	var ve *domain.ValidationError
	if errors.As(err, &ve) {
		return model.ValidationError{Field: ve.Field, Message: ve.Message, Path: inputPath(ve.Field)}, true
	}
	var be *domain.BudgetExceededError
	if errors.As(err, &be) {
		return model.BudgetExceededError{Remaining: be.Remaining, Requested: be.Requested, Message: be.Error()}, true
	}
	var se *domain.SelfApprovalNotAllowedError
	if errors.As(err, &se) {
		return model.SelfApprovalNotAllowedError{Message: se.Error(), Path: inputPath("id")}, true
	}
	var ie *app.IdempotencyKeyReusedError
	if errors.As(err, &ie) {
		return model.IdempotencyKeyReusedError{Message: ie.Error(), Path: inputPath("idempotencyKey")}, true
	}
	return nil, false
}

// inputPath is a UserError's path: the input field at fault, if any.
func inputPath(field string) []string {
	if field == "" {
		return nil
	}
	return []string{field}
}

// payload returns the business error as union member U, or the error itself
// when it is not a business rule or U cannot hold it.
func payload[U any](err error) (U, error) {
	var zero U
	member, ok := businessError(err)
	if !ok {
		return zero, err
	}
	u, ok := member.(U)
	if !ok {
		return zero, err
	}
	return u, nil
}

func pointers[T any](in []T) []*T {
	out := make([]*T, len(in))
	for i := range in {
		out[i] = &in[i]
	}
	return out
}

const (
	defaultPageSize = 20
	maxPageSize     = 100
)

// requestedPageSize validates "first" and applies the default and maximum.
func requestedPageSize(first *int) (int, error) {
	if first != nil && *first < 1 {
		return 0, fmt.Errorf("%w: first must be at least 1", domain.ErrInvalidInput)
	}
	return pageSize(first), nil
}

// listClaims asks the use case for one keyset page and shapes the connection.
func listClaims(ctx context.Context, svc *app.Services, f app.ClaimFilter, o app.ClaimOrder, first *int, after *string) (*model.ClaimConnection, error) {
	size, err := requestedPageSize(first)
	if err != nil {
		return nil, err
	}
	page := app.Page{First: size, Order: o}
	if after != nil && *after != "" {
		if page.After, err = decodeCursor(*after); err != nil {
			return nil, err
		}
	}
	res, err := svc.Claims.List(ctx, f, page)
	if err != nil {
		return nil, err
	}
	nodes := model.WrapClaims(res.Claims)
	edges := make([]*model.ClaimEdge, len(nodes))
	for i, n := range nodes {
		edges[i] = &model.ClaimEdge{Cursor: encodeCursor(app.NewClaimCursor(n.Claim, o)), Node: n}
	}
	info := &model.PageInfo{HasNextPage: res.HasNext}
	if len(edges) > 0 {
		info.EndCursor = &edges[len(edges)-1].Cursor
	}
	return &model.ClaimConnection{Edges: edges, PageInfo: info, TotalCount: res.Total}, nil
}

func rfc3339(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
