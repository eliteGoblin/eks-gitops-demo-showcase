package main

import (
	"fmt"

	gqlgen "github.com/99designs/gqlgen/graphql"

	"github.com/eliteGoblin/eks-gitops-demo/internal/adapter/graphql"
	"github.com/eliteGoblin/eks-gitops-demo/persisted"
)

// PERSISTED_QUERIES modes.
const (
	persistedOff       = "off"       // every request carries its document
	persistedAPQ       = "apq"       // a client may send a known document's hash instead; any document runs
	persistedAllowlist = "allowlist" // only the trusted documents in persisted/queries.json run
)

// persistedQueryPolicy returns the GraphQL extension for cfg's mode (nil for off).
// In allowlist mode introspection-only documents run only when both the
// playground and introspection are enabled (GraphiQL loads the schema).
func persistedQueryPolicy(cfg config) (gqlgen.HandlerExtension, error) {
	switch cfg.PersistedQueries {
	case persistedAPQ:
		return graphql.APQ(cfg.PersistedQueryCache), nil
	case persistedAllowlist:
		list, err := graphql.ParseAllowlist(persisted.Manifest)
		if err != nil {
			return nil, fmt.Errorf("trusted documents: %w", err)
		}
		return graphql.TrustedDocuments(list, cfg.Playground && cfg.Introspection), nil
	default:
		return nil, nil
	}
}
