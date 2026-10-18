package graphql_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gql "github.com/99designs/gqlgen/graphql"

	"github.com/eliteGoblin/eks-gitops-demo/internal/adapter/graphql"
	"github.com/eliteGoblin/eks-gitops-demo/internal/adapter/memory"
	"github.com/eliteGoblin/eks-gitops-demo/internal/app"
)

const meDoc = "query Me { me { subject } }"

func sha(s string) string {
	b := sha256.Sum256([]byte(s))
	return hex.EncodeToString(b[:])
}

// persistedServer is the test server with a persisted-query policy.
func persistedServer(t *testing.T, policy gql.HandlerExtension) http.Handler {
	t.Helper()
	st := memory.New()
	memory.Seed(st)
	return serverWithOptions(t, st, nil, func(o *graphql.Options) { o.Persisted = policy })
}

// post sends a raw GraphQL request body (query and/or extensions) as staff.
func post(t *testing.T, h http.Handler, body map[string]any) result {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/graphql", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(app.WithPrincipal(req.Context(), *staff))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var r result
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return r
}

func withHash(hash string, version any) map[string]any {
	return map[string]any{"persistedQuery": map[string]any{"version": version, "sha256Hash": hash}}
}

func wantData(t *testing.T, r result) {
	t.Helper()
	if len(r.Errors) > 0 || r.Data["me"] == nil {
		t.Fatalf("want data, got errors %+v data %v", r.Errors, r.Data)
	}
}

func wantCode(t *testing.T, r result, want string) {
	t.Helper()
	if got := code(r); got != want {
		t.Fatalf("want error code %s, got %q (errors %+v)", want, got, r.Errors)
	}
}

func TestAutomaticPersistedQueries(t *testing.T) {
	h := persistedServer(t, graphql.APQ(10))
	hash := sha(meDoc)

	// A hash the pod has never seen: the client must send the document once.
	wantCode(t, post(t, h, map[string]any{"extensions": withHash(hash, 1)}), graphql.CodePersistedQueryNotFound)
	// Document + hash registers it ...
	wantData(t, post(t, h, map[string]any{"query": meDoc, "extensions": withHash(hash, 1)}))
	// ... after which the hash alone is enough.
	wantData(t, post(t, h, map[string]any{"extensions": withHash(hash, 1)}))
	// Plain requests without the extension keep working.
	wantData(t, post(t, h, map[string]any{"query": meDoc}))
}

func TestAutomaticPersistedQueriesRejectBadHashesAsClientErrors(t *testing.T) {
	h := persistedServer(t, graphql.APQ(10))
	wantCode(t, post(t, h, map[string]any{"query": meDoc, "extensions": withHash(sha("something else"), 1)}), graphql.CodePersistedQueryInvalid)
	wantCode(t, post(t, h, map[string]any{"query": meDoc, "extensions": withHash(sha(meDoc), 2)}), graphql.CodePersistedQueryInvalid)
}

func TestTrustedDocuments(t *testing.T) {
	list, err := graphql.NewAllowlist(meDoc)
	if err != nil {
		t.Fatal(err)
	}
	canonical, hash, err := graphql.CanonicalDocument(meDoc)
	if err != nil {
		t.Fatal(err)
	}
	h := persistedServer(t, graphql.TrustedDocuments(list, false))

	t.Run("the trusted document runs, however it is spaced", func(t *testing.T) {
		wantData(t, post(t, h, map[string]any{"query": meDoc}))
		wantData(t, post(t, h, map[string]any{"query": "query Me {\n  me {\n    subject\n  }\n}\n# a comment\n"}))
		wantData(t, post(t, h, map[string]any{"query": canonical}))
	})
	t.Run("its hash alone is enough", func(t *testing.T) {
		wantData(t, post(t, h, map[string]any{"extensions": withHash(hash, 1)}))
	})
	t.Run("an ad-hoc document is refused", func(t *testing.T) {
		wantCode(t, post(t, h, map[string]any{"query": "{ me { subject roles } }"}), graphql.CodePersistedQueryNotAllowed)
	})
	t.Run("an unknown hash is refused, not asked for", func(t *testing.T) {
		wantCode(t, post(t, h, map[string]any{"extensions": withHash(sha("unknown"), 1)}), graphql.CodePersistedQueryNotAllowed)
	})
	t.Run("a hash that does not match the document is invalid", func(t *testing.T) {
		wantCode(t, post(t, h, map[string]any{"query": meDoc, "extensions": withHash(sha("other"), 1)}), graphql.CodePersistedQueryInvalid)
	})
	t.Run("a document that does not parse is refused without parser details", func(t *testing.T) {
		r := post(t, h, map[string]any{"query": "{ me {"})
		wantCode(t, r, graphql.CodePersistedQueryNotAllowed)
	})
	t.Run("introspection is refused unless the playground needs it", func(t *testing.T) {
		wantCode(t, post(t, h, map[string]any{"query": "{ __schema { queryType { name } } }"}), graphql.CodePersistedQueryNotAllowed)
	})
}

func TestTrustedDocumentsLetIntrospectionThroughForThePlayground(t *testing.T) {
	list, _ := graphql.NewAllowlist(meDoc)
	h := persistedServer(t, graphql.TrustedDocuments(list, true))

	introspection := `query IntrospectionQuery { __schema { ...Types } } fragment Types on __Schema { types { name } }`
	r := post(t, h, map[string]any{"query": introspection})
	if len(r.Errors) > 0 || r.Data["__schema"] == nil {
		t.Fatalf("introspection-only document must run: %+v", r.Errors)
	}
	// Smuggling a data field next to __schema is still refused.
	wantCode(t, post(t, h, map[string]any{"query": "{ __schema { queryType { name } } me { subject } }"}), graphql.CodePersistedQueryNotAllowed)
	// So is a mutation, even with only meta fields.
	wantCode(t, post(t, h, map[string]any{"query": "mutation { __typename }"}), graphql.CodePersistedQueryNotAllowed)
}

func TestAllowlistManifest(t *testing.T) {
	canonical, hash, err := graphql.CanonicalDocument(meDoc)
	if err != nil {
		t.Fatal(err)
	}
	good, _ := json.Marshal(map[string]string{hash: canonical})
	list, err := graphql.ParseAllowlist(good)
	if err != nil || list[hash] != canonical {
		t.Fatalf("valid manifest: %v %v", list, err)
	}

	for name, manifest := range map[string]string{
		"hash of another document": `{"` + sha("x") + `": ` + mustJSON(canonical) + `}`,
		"document not canonical":   `{"` + sha(meDoc) + `": ` + mustJSON(meDoc) + `}`,
		"empty":                    `{}`,
		"not json":                 `[`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := graphql.ParseAllowlist([]byte(manifest)); err == nil {
				t.Fatal("want an error")
			}
		})
	}
}

func TestCanonicalDocumentIgnoresFormattingAndComments(t *testing.T) {
	a, ha, err := graphql.CanonicalDocument("query Me{me{subject}}")
	if err != nil {
		t.Fatal(err)
	}
	b, hb, _ := graphql.CanonicalDocument("# comment\nquery Me {\n  me {\n    subject\n  }\n}\n")
	if a != b || ha != hb || ha != sha(a) {
		t.Fatalf("canonical forms differ:\n%q %s\n%q %s", a, ha, b, hb)
	}
	if _, _, err := graphql.CanonicalDocument("{ me {"); err == nil {
		t.Fatal("a document that does not parse has no canonical form")
	}
	if strings.Contains(a, "#") {
		t.Fatal("comments must not be part of the canonical form")
	}
}

// An approved document runs, and is cached, as its canonical text whatever
// padding or comments the client sent, so the parse cache can only ever hold
// allowlist documents.
func TestTrustedDocumentsRunTheCanonicalText(t *testing.T) {
	list, err := graphql.NewAllowlist(meDoc)
	if err != nil {
		t.Fatal(err)
	}
	canonical, _, _ := graphql.CanonicalDocument(meDoc)
	trusted, ok := graphql.TrustedDocuments(list, false).(gql.OperationParameterMutator)
	if !ok {
		t.Fatal("trusted documents must rewrite the operation parameters")
	}
	raw := &gql.RawParams{Query: "# padding\n" + meDoc + "\n# more padding " + strings.Repeat("z", 100)}
	if gerr := trusted.MutateOperationParameters(context.Background(), raw); gerr != nil {
		t.Fatal(gerr)
	}
	if raw.Query != canonical {
		t.Fatalf("ran %q, want the canonical document %q", raw.Query, canonical)
	}
}

// The allowlist's hashes are the SHA-256 of gqlparser's printed form. A
// gqlparser upgrade that prints differently changes every hash and breaks
// clients that send only hashes, so one canonical form is pinned here: when
// this fails after a dependency bump, regenerate the manifest and version it.
func TestCanonicalFormIsPinned(t *testing.T) {
	doc, hash, err := graphql.CanonicalDocument(`mutation SubmitClaim($input: SubmitClaimInput!) {
  submitClaim(input: $input) { __typename ... on Claim { id status } ... on UserError { message } }
}`)
	if err != nil {
		t.Fatal(err)
	}
	const (
		wantDoc  = pinnedCanonicalDoc
		wantHash = pinnedCanonicalHash
	)
	if doc != wantDoc || hash != wantHash {
		t.Fatalf("canonical form changed (regenerate the manifest):\ngot  %q\n     %s\nwant %q\n     %s", doc, hash, wantDoc, wantHash)
	}
}

func TestPersistedQueryCodesAreClientErrors(t *testing.T) {
	codes := graphql.ClientErrorCodes()
	for _, c := range []string{graphql.CodeDepthLimit, graphql.CodeDocumentTooLarge, graphql.CodePersistedQueryNotFound, graphql.CodePersistedQueryNotAllowed, graphql.CodePersistedQueryInvalid} {
		found := false
		for _, have := range codes {
			found = found || have == c
		}
		if !found {
			t.Errorf("%s must count as a client error, not a server failure", c)
		}
	}
}

func mustJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// The pinned canonical form of the SubmitClaim document in TestCanonicalFormIsPinned.
// Note gqlparser's quirk of a space between an operation's name and its
// variables: part of the form, so part of every hash.
const (
	pinnedCanonicalDoc  = "mutation SubmitClaim ($input: SubmitClaimInput!) {\n  submitClaim(input: $input) {\n    __typename\n    ... on Claim {\n      id\n      status\n    }\n    ... on UserError {\n      message\n    }\n  }\n}\n"
	pinnedCanonicalHash = "452c8ed16b8faf07ec2f3b7b0c7d12d9e991954f3d3959fc355be1f6123c3f10"
)
