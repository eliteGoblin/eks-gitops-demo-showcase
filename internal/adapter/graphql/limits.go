package graphql

import (
	"context"
	"strings"

	gql "github.com/99designs/gqlgen/graphql"
	"github.com/99designs/gqlgen/graphql/errcode"
	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/gqlerror"
)

// CodeDepthLimit is the extensions.code of an operation nested too deeply.
const CodeDepthLimit = "DEPTH_LIMIT_EXCEEDED"

// CodeDocumentTooLarge is the extensions.code of a document over the size limit.
const CodeDocumentTooLarge = "DOCUMENT_TOO_LARGE"

// DefaultMaxDocumentBytes bounds one GraphQL document unless Options says
// otherwise. The demo's largest document is about 1 KiB.
const DefaultMaxDocumentBytes = 32 << 10

// documentSizeLimit refuses a document over max bytes before anything parses
// or keeps it. Documents are cached by their text (the parsed-query cache and
// the APQ cache hold up to 1000 each), so this bound, not the request body
// limit, decides how much memory one client can pin.
type documentSizeLimit struct{ max int }

var _ interface {
	gql.HandlerExtension
	gql.OperationParameterMutator
} = documentSizeLimit{}

func (documentSizeLimit) ExtensionName() string               { return "DocumentSizeLimit" }
func (documentSizeLimit) Validate(gql.ExecutableSchema) error { return nil }

func (d documentSizeLimit) MutateOperationParameters(_ context.Context, raw *gql.RawParams) *gqlerror.Error {
	if len(raw.Query) <= d.max {
		return nil
	}
	err := gqlerror.Errorf("the document is larger than %d bytes", d.max)
	errcode.Set(err, CodeDocumentTooLarge)
	return err
}

// depthLimit rejects operations whose field nesting exceeds max. The schema is
// cyclic (claim → plan → participant → plans → claims …), so without it a short
// query can fan out into thousands of resolver calls. Introspection fields
// (__schema, __type) are not counted: GraphiQL's introspection query is deep
// by design and is switched off outside development anyway.
type depthLimit struct{ max int }

var (
	_ gql.HandlerExtension        = depthLimit{}
	_ gql.OperationContextMutator = depthLimit{}
)

func (depthLimit) ExtensionName() string               { return "DepthLimit" }
func (depthLimit) Validate(gql.ExecutableSchema) error { return nil }

func (d depthLimit) MutateOperationContext(_ context.Context, rc *gql.OperationContext) *gqlerror.Error {
	if rc.Operation == nil {
		return nil
	}
	if depth := selectionDepth(rc.Operation.SelectionSet); depth > d.max {
		err := gqlerror.Errorf("operation depth %d exceeds the limit of %d", depth, d.max)
		errcode.Set(err, CodeDepthLimit)
		return err
	}
	return nil
}

// selectionDepth returns the deepest field nesting in set. Fragments add no
// depth of their own; their fields count where the fragment is used.
func selectionDepth(set ast.SelectionSet) int {
	deepest := 0
	for _, sel := range set {
		var depth int
		switch s := sel.(type) {
		case *ast.Field:
			if strings.HasPrefix(s.Name, "__") {
				continue
			}
			depth = 1 + selectionDepth(s.SelectionSet)
		case *ast.InlineFragment:
			depth = selectionDepth(s.SelectionSet)
		case *ast.FragmentSpread:
			if s.Definition != nil {
				depth = selectionDepth(s.Definition.SelectionSet)
			}
		}
		deepest = max(deepest, depth)
	}
	return deepest
}

// connectionCost charges a paginated field for every item it may return, so
// first: 100 costs five times first: 20 and the complexity limit bounds the
// real amount of work, not just the size of the query text.
func connectionCost(childComplexity int, first *int) int {
	return 1 + pageSize(first)*childComplexity
}

func pageSize(first *int) int {
	if first == nil {
		return defaultPageSize
	}
	return min(max(*first, 1), maxPageSize)
}
