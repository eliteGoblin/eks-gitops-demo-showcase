package app

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/eliteGoblin/eks-gitops-demo/internal/domain"
)

// Search bounds.
const (
	minSearchTerm    = 2
	maxSearchTerm    = 100
	MaxSearchResults = 50
)

// SearchResults are the records a search matched, each group in its own
// order; together they hold at most the number asked for.
type SearchResults struct {
	Participants []domain.Participant
	Providers    []domain.Provider
	Claims       []domain.Claim
}

// SearchService finds participants, providers and claims by text, within
// what the caller may read.
type SearchService struct {
	participants ParticipantRepository
	providers    ProviderRepository
	claims       ClaimRepository
	people       *ParticipantService
	guard        guard
}

// Search returns up to first records matching term: participants by name or
// member number, then providers by name or ABN, then claims by id prefix,
// filling each group in that order. The policy's scopes are applied in the
// queries, so nothing the caller may not read is loaded, and participants
// are field-masked as participant(id) masks them. Each search is audited
// once, with counts but without the term, which may be a person's name.
func (s *SearchService) Search(ctx context.Context, term string, first int) (SearchResults, error) {
	term = strings.TrimSpace(term)
	if !utf8.ValidString(term) || strings.ContainsRune(term, 0) {
		return SearchResults{}, fmt.Errorf("%w: term must be text (valid UTF-8, no NUL)", domain.ErrInvalidInput)
	}
	if n := utf8.RuneCountInString(term); n < minSearchTerm || n > maxSearchTerm {
		return SearchResults{}, fmt.Errorf("%w: term must be %d to %d characters", domain.ErrInvalidInput, minSearchTerm, maxSearchTerm)
	}
	if first < 1 || first > MaxSearchResults {
		return SearchResults{}, fmt.Errorf("%w: first must be 1 to %d", domain.ErrInvalidInput, MaxSearchResults)
	}
	pr, err := s.guard.principal(ctx)
	if err != nil {
		return SearchResults{}, err
	}
	var res SearchResults
	if res.Participants, err = s.findParticipants(ctx, pr, term, first); err != nil {
		return SearchResults{}, err
	}
	if left := first - len(res.Participants); left > 0 {
		if res.Providers, err = s.providers.SearchProviders(ctx, term, left); err != nil {
			return SearchResults{}, fmt.Errorf("search providers: %w", err)
		}
	}
	if left := first - len(res.Participants) - len(res.Providers); left > 0 {
		if res.Claims, err = s.findClaims(ctx, pr, term, left); err != nil {
			return SearchResults{}, err
		}
	}
	s.guard.audit.Record(ctx, AuditEvent{Actor: pr.Subject, Username: pr.Username, Roles: pr.RoleNames(), Action: ActionSearch, Outcome: "allow",
		Detail: fmt.Sprintf("%d participants, %d providers, %d claims", len(res.Participants), len(res.Providers), len(res.Claims))})
	return res, nil
}

func (s *SearchService) findParticipants(ctx context.Context, pr Principal, term string, limit int) ([]domain.Participant, error) {
	scope, err := s.guard.policy.ParticipantScope(ctx, pr)
	if err != nil || scope.Empty() {
		return nil, err
	}
	found, err := s.participants.SearchParticipants(ctx, term, scope, limit)
	if err != nil {
		return nil, fmt.Errorf("search participants: %w", err)
	}
	for i := range found {
		masked, err := s.people.masked(ctx, pr, found[i])
		if err != nil {
			return nil, err
		}
		found[i] = *masked
	}
	return found, nil
}

func (s *SearchService) findClaims(ctx context.Context, pr Principal, term string, limit int) ([]domain.Claim, error) {
	scope, err := s.guard.policy.ClaimScope(ctx, pr)
	if err != nil || scope.Empty() {
		return nil, err
	}
	claims, err := s.claims.QueryClaims(ctx, ClaimQuery{Scope: scope, IDPrefix: term, Limit: limit})
	if err != nil {
		return nil, fmt.Errorf("search claims: %w", err)
	}
	return claims, nil
}
