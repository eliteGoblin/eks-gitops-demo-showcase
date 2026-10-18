package graphql

import (
	"context"
	"time"

	"github.com/vikstrous/dataloadgen"

	"github.com/eliteGoblin/eks-gitops-demo/internal/app"
	"github.com/eliteGoblin/eks-gitops-demo/internal/domain"
)

// loaders batch the reads behind nested fields (Claim.plan, Claim.provider,
// Plan.participant, ...) into one repository call per field level, instead
// of one per item (the N+1 problem). They live for one response (a query, a
// mutation or one subscription event), so cached results never cross callers
// or go stale. The use cases still authorize every record.
type loaders struct {
	plans        *dataloadgen.Loader[string, *domain.Plan]
	participants *dataloadgen.Loader[string, *domain.Participant]
	providers    *dataloadgen.Loader[string, *domain.Provider]
}

// batchWait is how long a loader collects keys before it fetches.
const batchWait = time.Millisecond

func newLoaders(svc *app.Services) *loaders {
	return &loaders{
		plans:        dataloadgen.NewLoader(batch(svc.Plans.GetMany), dataloadgen.WithWait(batchWait)),
		participants: dataloadgen.NewLoader(batch(svc.Participants.GetMany), dataloadgen.WithWait(batchWait)),
		providers:    dataloadgen.NewLoader(batch(svc.Providers.GetMany), dataloadgen.WithWait(batchWait)),
	}
}

// batch adapts a use case's GetMany (values aligned with keys, nil = hidden)
// to a loader fetch; one error fails the whole batch.
func batch[T any](getMany func(context.Context, []string) ([]*T, error)) func(context.Context, []string) ([]*T, []error) {
	return func(ctx context.Context, keys []string) ([]*T, []error) {
		values, err := getMany(ctx, keys)
		if err != nil {
			return nil, []error{err}
		}
		return values, nil
	}
}

type loadersKey struct{}

func withLoaders(ctx context.Context, l *loaders) context.Context {
	return context.WithValue(ctx, loadersKey{}, l)
}

func loadersFrom(ctx context.Context) *loaders {
	return ctx.Value(loadersKey{}).(*loaders) //nolint:forcetypeassert // set by NewHandler for every response
}

// required turns "hidden or missing" into ErrForbidden for a non-null field.
func required[T any](v *T, err error) (*T, error) {
	if err != nil {
		return nil, err
	}
	if v == nil {
		return nil, app.ErrForbidden
	}
	return v, nil
}

// optionalRef loads an optional reference; an empty id is a null field.
func optionalRef[T any](ctx context.Context, l *dataloadgen.Loader[string, *T], id string) (*T, error) {
	if id == "" {
		return nil, nil
	}
	return l.Load(ctx, id)
}
