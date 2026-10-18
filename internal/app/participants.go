package app

import (
	"context"

	"github.com/eliteGoblin/eks-gitops-demo/internal/domain"
)

// ParticipantService reads participants, masking fields the caller may not see.
type ParticipantService struct {
	repo  ParticipantRepository
	guard guard
}

// Get returns the participant with field masking applied, or nil when it does
// not exist or the caller may not read it.
func (s *ParticipantService) Get(ctx context.Context, id string) (*domain.Participant, error) {
	pr, err := s.guard.authorize(ctx, ActionReadParticipant, Resource{ParticipantID: id})
	if err != nil {
		return nil, hideDenied(err)
	}
	p, err := s.repo.GetParticipant(ctx, id)
	if err != nil {
		return nil, hideDenied(err)
	}
	return s.masked(ctx, pr, p)
}

// GetMany returns the participants for ids in the same order, nil where one
// does not exist or may not be read, with field masking applied. It reads
// the repository once (DataLoaders).
func (s *ParticipantService) GetMany(ctx context.Context, ids []string) ([]*domain.Participant, error) {
	if _, err := s.guard.principal(ctx); err != nil {
		return nil, err
	}
	found, err := s.repo.GetParticipants(ctx, ids)
	if err != nil {
		return nil, err
	}
	shown, err := s.visible(ctx, found)
	if err != nil {
		return nil, err
	}
	return alignTo(ids, found, shown, participantID), nil
}

// visible returns, aligned to found, each participant as the caller may see
// it (field-masked), or nil where it may not be read.
func (s *ParticipantService) visible(ctx context.Context, found []domain.Participant) ([]*domain.Participant, error) {
	pr, err := s.guard.principal(ctx)
	if err != nil {
		return nil, err
	}
	shown, err := visibleOf(ctx, s.guard, found, func(p domain.Participant) (Action, Resource) {
		return ActionReadParticipant, Resource{ParticipantID: p.ID}
	})
	if err != nil {
		return nil, err
	}
	for i, p := range shown {
		if p == nil {
			continue
		}
		if shown[i], err = s.masked(ctx, pr, *p); err != nil {
			return nil, err
		}
	}
	return shown, nil
}

func participantID(p domain.Participant) string { return p.ID }

// masked applies the date-of-birth rule to a copy of p.
func (s *ParticipantService) masked(ctx context.Context, pr Principal, p domain.Participant) (*domain.Participant, error) {
	seeDOB, err := s.guard.policy.CanSeeDateOfBirth(ctx, pr, p.ID)
	if err != nil {
		return nil, err
	}
	if !seeDOB {
		p.DateOfBirth = nil // p is a copy; the stored record is untouched
	}
	return &p, nil
}
