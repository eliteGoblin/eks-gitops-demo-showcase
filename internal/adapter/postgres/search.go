package postgres

import (
	"context"

	"github.com/eliteGoblin/eks-gitops-demo/internal/adapter/postgres/sqlcdb"
	"github.com/eliteGoblin/eks-gitops-demo/internal/app"
	"github.com/eliteGoblin/eks-gitops-demo/internal/domain"
)

// containsPattern is the LIKE pattern matching s anywhere, literally.
func containsPattern(s string) string { return "%" + likeEscape(s) + "%" }

func rowLimit(limit int) int32 {
	if limit < 1 || limit > maxLimit {
		limit = maxLimit
	}
	return int32(limit) //nolint:gosec // bounded by maxLimit
}

func (s *Store) SearchParticipants(ctx context.Context, term string, scope app.Scope, limit int) ([]domain.Participant, error) {
	rows, err := s.q.SearchParticipants(ctx, sqlcdb.SearchParticipantsParams{
		AllScope: scope.All, Ids: nonNil(scope.ParticipantIDs), Pattern: containsPattern(term), RowLimit: rowLimit(limit),
	})
	return mapRows(rows, toParticipant), err
}

func (s *Store) SearchProviders(ctx context.Context, term string, limit int) ([]domain.Provider, error) {
	rows, err := s.q.SearchProviders(ctx, sqlcdb.SearchProvidersParams{Pattern: containsPattern(term), RowLimit: rowLimit(limit)})
	return mapRows(rows, toProvider), err
}

func (s *Store) BookedParticipants(ctx context.Context, providerID string) ([]string, error) {
	return s.q.BookedParticipants(ctx, providerID)
}
