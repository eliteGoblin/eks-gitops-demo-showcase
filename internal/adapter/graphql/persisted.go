package graphql

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	gql "github.com/99designs/gqlgen/graphql"
	"github.com/99designs/gqlgen/graphql/errcode"
	"github.com/99designs/gqlgen/graphql/handler/extension"
	"github.com/99designs/gqlgen/graphql/handler/lru"
	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/formatter"
	"github.com/vektah/gqlparser/v2/gqlerror"
	"github.com/vektah/gqlparser/v2/parser"
)

// Error codes of the persisted-query layer. All are the client's to fix.
const (
	CodePersistedQueryNotFound   = "PERSISTED_QUERY_NOT_FOUND"   // APQ: unknown hash; send the document with it once
	CodePersistedQueryNotAllowed = "PERSISTED_QUERY_NOT_ALLOWED" // trusted documents: not on the allowlist
	CodePersistedQueryInvalid    = "PERSISTED_QUERY_INVALID"     // malformed extension, or a hash that is not the document's
)

// ClientErrorCodes are this adapter's error codes that mean "the caller's
// mistake", so telemetry counts them as client errors, not server failures.
func ClientErrorCodes() []string {
	return []string{CodeDepthLimit, CodeDocumentTooLarge, CodePersistedQueryNotFound, CodePersistedQueryNotAllowed, CodePersistedQueryInvalid}
}

// APQ returns Automatic Persisted Queries (Apollo protocol): a client may send
// only a document's SHA-256 (extensions.persistedQuery.sha256Hash). Each pod
// remembers up to size documents; on a miss (an unknown hash, or a replica
// that has not seen it) the client gets PERSISTED_QUERY_NOT_FOUND and sends the
// document with the hash once. Any document may still run.
func APQ(size int) gql.HandlerExtension {
	return apq{inner: extension.AutomaticPersistedQuery{Cache: lru.New[string](size)}}
}

type apq struct {
	inner extension.AutomaticPersistedQuery
}

var _ interface {
	gql.HandlerExtension
	gql.OperationParameterMutator
} = apq{}

func (apq) ExtensionName() string { return "AutomaticPersistedQuery" }

func (a apq) Validate(schema gql.ExecutableSchema) error { return a.inner.Validate(schema) }

// MutateOperationParameters delegates to gqlgen and gives every error a code:
// gqlgen leaves a bad hash or version without one, which telemetry would
// otherwise count as a server failure.
func (a apq) MutateOperationParameters(ctx context.Context, raw *gql.RawParams) *gqlerror.Error {
	err := a.inner.MutateOperationParameters(ctx, raw)
	if err != nil {
		if _, coded := err.Extensions["code"]; !coded {
			errcode.Set(err, CodePersistedQueryInvalid)
		}
	}
	return err
}

// Allowlist maps the SHA-256 of a canonical document to that document.
type Allowlist map[string]string

// NewAllowlist builds an allowlist from documents in any formatting.
func NewAllowlist(docs ...string) (Allowlist, error) {
	list := Allowlist{}
	for _, d := range docs {
		doc, hash, err := CanonicalDocument(d)
		if err != nil {
			return nil, err
		}
		list[hash] = doc
	}
	return list, nil
}

// ParseAllowlist reads a manifest (hash → canonical document) and checks every
// entry, so a hand-edited or stale manifest fails at startup instead of never
// matching a request.
func ParseAllowlist(manifest []byte) (Allowlist, error) {
	var list Allowlist
	if err := json.Unmarshal(manifest, &list); err != nil {
		return nil, fmt.Errorf("allowlist manifest: %w", err)
	}
	if len(list) == 0 {
		return nil, errors.New("allowlist manifest has no documents")
	}
	for hash, doc := range list {
		canonical, want, err := CanonicalDocument(doc)
		if err != nil {
			return nil, fmt.Errorf("allowlist document %s: %w", hash, err)
		}
		if canonical != doc || want != hash {
			return nil, fmt.Errorf("allowlist entry %s is not the canonical document and its SHA-256 (regenerate the manifest)", hash)
		}
	}
	return list, nil
}

// CanonicalDocument parses a GraphQL document and prints it in one canonical
// form (formatting and comments dropped), so a document hashes the same
// however a client spaced it. The hash is the hex SHA-256 of the canonical form.
func CanonicalDocument(query string) (doc, hash string, err error) {
	parsed, err := parser.ParseQuery(&ast.Source{Input: query})
	if err != nil {
		return "", "", err
	}
	doc = canonicalText(parsed)
	return doc, sha256Hex(doc), nil
}

func canonicalText(doc *ast.QueryDocument) string {
	var b strings.Builder
	formatter.NewFormatter(&b, formatter.WithIndent("  ")).FormatQueryDocument(doc)
	return b.String()
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// TrustedDocuments runs only documents on the allowlist (the production
// posture for first-party clients, whose documents are known at build time):
// a client sends a trusted document, or just its hash; anything else gets
// PERSISTED_QUERY_NOT_ALLOWED. allowIntrospection lets introspection-only
// queries through so GraphiQL can load the schema in development.
func TrustedDocuments(list Allowlist, allowIntrospection bool) gql.HandlerExtension {
	return trustedDocuments{list: list, allowIntrospection: allowIntrospection}
}

type trustedDocuments struct {
	list               Allowlist
	allowIntrospection bool
}

var _ interface {
	gql.HandlerExtension
	gql.OperationParameterMutator
} = trustedDocuments{}

func (trustedDocuments) ExtensionName() string { return "TrustedDocuments" }

func (t trustedDocuments) Validate(gql.ExecutableSchema) error {
	if len(t.list) == 0 {
		return errors.New("trusted documents: the allowlist is empty")
	}
	return nil
}

// MutateOperationParameters admits a request only when its document is on the
// allowlist: sent in full (any formatting) or by its hash.
func (t trustedDocuments) MutateOperationParameters(_ context.Context, raw *gql.RawParams) *gqlerror.Error {
	hash, hasHash, gerr := persistedHash(raw.Extensions)
	if gerr != nil {
		return gerr
	}
	if raw.Query == "" {
		if !hasHash {
			return nil // nothing to run: gqlgen reports the missing document
		}
		doc, ok := t.list[hash]
		if !ok {
			return persistedError(CodePersistedQueryNotAllowed, "unknown document hash: only trusted documents run")
		}
		raw.Query = doc
		return nil
	}
	parsed, err := parser.ParseQuery(&ast.Source{Input: raw.Query})
	if err != nil {
		return notAllowed()
	}
	canonicalHash := sha256Hex(canonicalText(parsed))
	if hasHash && hash != canonicalHash && hash != sha256Hex(raw.Query) {
		return persistedError(CodePersistedQueryInvalid, "the hash does not match the document")
	}
	if canonical, ok := t.list[canonicalHash]; ok {
		// Run, and cache, exactly the trusted text whatever padding the client
		// sent: the parse cache then only ever holds allowlist documents.
		raw.Query = canonical
		return nil
	}
	if t.allowIntrospection && introspectionOnly(parsed) {
		return nil
	}
	return notAllowed()
}

func notAllowed() *gqlerror.Error {
	return persistedError(CodePersistedQueryNotAllowed, "only trusted documents run on this server")
}

// persistedHash reads extensions.persistedQuery (version 1 only).
func persistedHash(ext map[string]any) (hash string, ok bool, err *gqlerror.Error) {
	v, present := ext["persistedQuery"]
	if !present || v == nil {
		return "", false, nil
	}
	pq, isMap := v.(map[string]any)
	if !isMap {
		return "", false, persistedError(CodePersistedQueryInvalid, "invalid persistedQuery extension")
	}
	if version, ok := wholeNumber(pq["version"]); !ok || version != 1 {
		return "", false, persistedError(CodePersistedQueryInvalid, "unsupported persistedQuery version")
	}
	hash, _ = pq["sha256Hash"].(string)
	if hash == "" {
		return "", false, persistedError(CodePersistedQueryInvalid, "persistedQuery.sha256Hash is missing")
	}
	return hash, true, nil
}

// wholeNumber reads a JSON number however the transport decoded it.
func wholeNumber(v any) (int64, bool) {
	switch n := v.(type) {
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	case float64:
		return int64(n), n == float64(int64(n))
	case int:
		return int64(n), true
	case int64:
		return n, true
	default:
		return 0, false
	}
}

func persistedError(code, msg string) *gqlerror.Error {
	err := gqlerror.Errorf("%s", msg)
	errcode.Set(err, code)
	return err
}

// introspectionOnly reports whether every operation is a query that selects
// only meta fields (__schema, __type, __typename), directly or through fragments.
func introspectionOnly(doc *ast.QueryDocument) bool {
	if len(doc.Operations) == 0 {
		return false
	}
	for _, op := range doc.Operations {
		if op.Operation != ast.Query || !metaFieldsOnly(op.SelectionSet, doc, map[string]bool{}) {
			return false
		}
	}
	return true
}

func metaFieldsOnly(set ast.SelectionSet, doc *ast.QueryDocument, seen map[string]bool) bool {
	for _, sel := range set {
		switch s := sel.(type) {
		case *ast.Field:
			if s.Name != "__schema" && s.Name != "__type" && s.Name != "__typename" {
				return false
			}
		case *ast.InlineFragment:
			if !metaFieldsOnly(s.SelectionSet, doc, seen) {
				return false
			}
		case *ast.FragmentSpread:
			if seen[s.Name] {
				continue
			}
			seen[s.Name] = true
			f := doc.Fragments.ForName(s.Name)
			if f == nil || !metaFieldsOnly(f.SelectionSet, doc, seen) {
				return false
			}
		default:
			return false
		}
	}
	return true
}
