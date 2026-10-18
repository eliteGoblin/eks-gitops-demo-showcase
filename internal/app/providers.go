package app

import (
	"context"

	"github.com/eliteGoblin/eks-gitops-demo/internal/domain"
)

// ProviderService reads provider records.
type ProviderService struct{ repo ProviderRepository }

// Get returns a provider. Provider name and ABN are public business data, so
// no relationship check applies, but the caller must be authenticated.
func (s *ProviderService) Get(ctx context.Context, id string) (*domain.Provider, error) {
	if _, ok := PrincipalFrom(ctx); !ok {
		return nil, ErrUnauthenticated
	}
	p, err := s.repo.GetProvider(ctx, id)
	if err != nil {
		return nil, hideDenied(err)
	}
	return &p, nil
}

// GetMany returns the providers for ids in the same order, nil where one does
// not exist. It reads the repository once (DataLoaders).
func (s *ProviderService) GetMany(ctx context.Context, ids []string) ([]*domain.Provider, error) {
	found, err := s.findMany(ctx, ids)
	if err != nil {
		return nil, err
	}
	shown, err := s.visible(ctx, found)
	if err != nil {
		return nil, err
	}
	return alignTo(ids, found, shown, providerID), nil
}

func (s *ProviderService) findMany(ctx context.Context, ids []string) ([]domain.Provider, error) {
	if _, ok := PrincipalFrom(ctx); !ok {
		return nil, ErrUnauthenticated
	}
	return s.repo.GetProviders(ctx, ids)
}

// visible returns, aligned to found, every provider: provider records are
// public business data to any authenticated caller.
func (s *ProviderService) visible(ctx context.Context, found []domain.Provider) ([]*domain.Provider, error) {
	if _, ok := PrincipalFrom(ctx); !ok {
		return nil, ErrUnauthenticated
	}
	out := make([]*domain.Provider, len(found))
	for i := range found {
		out[i] = &found[i]
	}
	return out, nil
}

func providerID(p domain.Provider) string { return p.ID }
