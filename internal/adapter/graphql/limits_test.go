package graphql_test

import (
	"strings"
	"testing"

	"github.com/eliteGoblin/eks-gitops-demo/internal/adapter/graphql"
	"github.com/eliteGoblin/eks-gitops-demo/internal/adapter/memory"
)

// Documents are cached by their text (the parsed-query cache and the APQ
// cache), so an oversized one is refused before anything parses or keeps it:
// junk reports DOCUMENT_TOO_LARGE rather than a parse error, and its hash is
// never remembered.
func TestOversizedDocumentsAreRefusedBeforeParsingOrCaching(t *testing.T) {
	h := persistedServer(t, graphql.APQ(10))
	junk := "{" + strings.Repeat("x", graphql.DefaultMaxDocumentBytes) // does not parse
	wantCode(t, post(t, h, map[string]any{"query": junk, "extensions": withHash(sha(junk), 1)}), graphql.CodeDocumentTooLarge)
	wantCode(t, post(t, h, map[string]any{"extensions": withHash(sha(junk), 1)}), graphql.CodePersistedQueryNotFound)

	open := persistedServer(t, nil)
	padded := meDoc + "\n#" + strings.Repeat("y", graphql.DefaultMaxDocumentBytes)
	wantCode(t, post(t, open, map[string]any{"query": padded}), graphql.CodeDocumentTooLarge)
	fits := meDoc + "\n#" + strings.Repeat("y", graphql.DefaultMaxDocumentBytes-len(meDoc)-2)
	wantData(t, post(t, open, map[string]any{"query": fits}))

	list, err := graphql.NewAllowlist(meDoc)
	if err != nil {
		t.Fatal(err)
	}
	trusted := persistedServer(t, graphql.TrustedDocuments(list, false))
	wantCode(t, post(t, trusted, map[string]any{"query": padded}), graphql.CodeDocumentTooLarge)
}

func TestDocumentSizeLimitIsConfigurable(t *testing.T) {
	st := memory.New()
	memory.Seed(st)
	h := serverWithOptions(t, st, nil, func(o *graphql.Options) { o.MaxDocumentBytes = 64 })
	wantData(t, post(t, h, map[string]any{"query": meDoc}))
	wantCode(t, post(t, h, map[string]any{"query": meDoc + "\n#" + strings.Repeat("y", 64)}), graphql.CodeDocumentTooLarge)
}
