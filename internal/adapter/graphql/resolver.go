// Package graphql is the inbound GraphQL adapter. Resolvers are thin: they
// convert input, call a use case and map the result; authorization and audit
// live in the app layer.
package graphql

import "github.com/eliteGoblin/eks-gitops-demo/internal/app"

type Resolver struct {
	Svc *app.Services
}
