// Package memory implements the app's repository ports with Go maps. It backs
// unit tests and quick local runs; it follows the same contract as the
// Postgres adapter (see the forEachStore tests in internal/app).
package memory

import (
	"context"
	"slices"
	"sort"
	"sync"

	"github.com/eliteGoblin/eks-gitops-demo/internal/app"
	"github.com/eliteGoblin/eks-gitops-demo/internal/domain"
)

// Store implements every repository port and app.Relationships.
type Store struct {
	mu           sync.RWMutex
	participants map[string]domain.Participant
	plans        map[string]domain.Plan
	providers    map[string]domain.Provider
	claims       map[string]domain.Claim
	requests     map[string]domain.SupportRequest
	delegations  []domain.Delegation
	bookings     []domain.ServiceBooking
	idempotency  map[idempotencyID]app.IdempotencyRecord
}

// idempotencyID is an idempotency key in its caller's scope.
type idempotencyID struct{ subject, key string }

// New returns an empty store; call Seed to load the demo data.
func New() *Store {
	return &Store{
		participants: map[string]domain.Participant{},
		plans:        map[string]domain.Plan{},
		providers:    map[string]domain.Provider{},
		claims:       map[string]domain.Claim{},
		requests:     map[string]domain.SupportRequest{},
		idempotency:  map[idempotencyID]app.IdempotencyRecord{},
	}
}

// getMany returns the entities of ids that exist, cloned by clone.
func getMany[T any](mu *sync.RWMutex, m map[string]T, ids []string, clone func(T) T) []T {
	mu.RLock()
	defer mu.RUnlock()
	out := []T{}
	for _, id := range ids {
		if v, ok := m[id]; ok {
			out = append(out, clone(v))
		}
	}
	return out
}

func cloneParticipant(p domain.Participant) domain.Participant {
	if p.DateOfBirth != nil {
		dob := *p.DateOfBirth // never share the stored pointer
		p.DateOfBirth = &dob
	}
	return p
}

func clonePlan(p domain.Plan) domain.Plan {
	p.Allocations = slices.Clone(p.Allocations)
	return p
}

func identity[T any](v T) T { return v }

func get[T any](mu *sync.RWMutex, m map[string]T, id string) (T, error) {
	mu.RLock()
	defer mu.RUnlock()
	v, ok := m[id]
	if !ok {
		var zero T
		return zero, domain.ErrNotFound
	}
	return v, nil
}

func (s *Store) GetParticipant(_ context.Context, id string) (domain.Participant, error) {
	p, err := get(&s.mu, s.participants, id)
	return cloneParticipant(p), err
}

func (s *Store) GetParticipants(_ context.Context, ids []string) ([]domain.Participant, error) {
	return getMany(&s.mu, s.participants, ids, cloneParticipant), nil
}

func (s *Store) GetPlan(_ context.Context, id string) (domain.Plan, error) {
	p, err := get(&s.mu, s.plans, id)
	return clonePlan(p), err
}

func (s *Store) GetPlans(_ context.Context, ids []string) ([]domain.Plan, error) {
	return getMany(&s.mu, s.plans, ids, clonePlan), nil
}

func (s *Store) ListPlansByParticipant(_ context.Context, participantID string) ([]domain.Plan, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []domain.Plan{}
	for _, p := range s.plans {
		if p.ParticipantID == participantID {
			p.Allocations = slices.Clone(p.Allocations)
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *Store) GetProvider(_ context.Context, id string) (domain.Provider, error) {
	return get(&s.mu, s.providers, id)
}

func (s *Store) GetProviders(_ context.Context, ids []string) ([]domain.Provider, error) {
	return getMany(&s.mu, s.providers, ids, identity), nil
}

func (s *Store) GetClaim(_ context.Context, id string) (domain.Claim, error) {
	c, err := get(&s.mu, s.claims, id)
	return cloneClaim(c), err
}

func (s *Store) GetClaims(_ context.Context, ids []string) ([]domain.Claim, error) {
	return getMany(&s.mu, s.claims, ids, cloneClaim), nil
}

// cloneClaim copies the payment too, so callers never share stored state.
func cloneClaim(c domain.Claim) domain.Claim {
	if c.Payment != nil {
		p := *c.Payment
		c.Payment = &p
	}
	return c
}

func (s *Store) ClaimsOfPlan(_ context.Context, planID string) ([]domain.Claim, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.claimsOfPlan(planID), nil
}

func (s *Store) inScope(scope app.Scope, c domain.Claim) bool {
	if scope.All || (scope.ProviderID != "" && c.ProviderID == scope.ProviderID) {
		return true
	}
	plan, ok := s.plans[c.PlanID]
	return ok && slices.Contains(scope.ParticipantIDs, plan.ParticipantID)
}

// claimsOfPlan returns the claims of planID (all plans when empty), ordered by
// SubmittedAt then ID. The caller holds s.mu.
func (s *Store) claimsOfPlan(planID string) []domain.Claim {
	out := []domain.Claim{}
	for _, c := range s.claims {
		if planID == "" || c.PlanID == planID {
			out = append(out, cloneClaim(c))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].SubmittedAt.Equal(out[j].SubmittedAt) {
			return out[i].SubmittedAt.Before(out[j].SubmittedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// InsertClaimWithinBudget runs check and the insert under one write lock, so
// no other budget write can happen between them.
func (s *Store) InsertClaimWithinBudget(_ context.Context, c domain.Claim, check app.BudgetCheck) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := check(s.claimsOfPlan(c.PlanID), s.bookingsOfPlan(c.PlanID)); err != nil {
		return err
	}
	s.claims[c.ID] = cloneClaim(c)
	return nil
}

// UpdateClaim is the conditional transition: the stored status must still be
// from. It stores c's status and payment.
func (s *Store) UpdateClaim(_ context.Context, c domain.Claim, from domain.ClaimStatus) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, ok := s.claims[c.ID]
	if !ok {
		return domain.ErrNotFound
	}
	if stored.Status != from {
		return domain.ClaimNotInStatus(from)
	}
	updated := cloneClaim(c)
	stored.Status, stored.Payment = updated.Status, updated.Payment
	s.claims[c.ID] = stored
	return nil
}

func (s *Store) IsActiveDelegate(_ context.Context, nomineeID, participantID string) (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return slices.ContainsFunc(s.delegations, func(d domain.Delegation) bool {
		return d.Active && d.NomineeID == nomineeID && d.ParticipantID == participantID
	}), nil
}

func (s *Store) DelegatedParticipants(_ context.Context, nomineeID string) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []string
	for _, d := range s.delegations {
		if d.Active && d.NomineeID == nomineeID {
			out = append(out, d.ParticipantID)
		}
	}
	return out, nil
}

func (s *Store) BookingsOfPlan(_ context.Context, planID string) ([]domain.ServiceBooking, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.bookingsOfPlan(planID), nil
}

// bookingsOfPlan returns copies of the plan's bookings. The caller holds s.mu.
func (s *Store) bookingsOfPlan(planID string) []domain.ServiceBooking {
	out := []domain.ServiceBooking{}
	for _, b := range s.bookings {
		if b.PlanID == planID {
			out = append(out, b)
		}
	}
	return out
}

func (s *Store) BookedCategories(_ context.Context, providerID, planID string) ([]domain.Category, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []domain.Category
	for _, b := range s.bookings {
		if b.ProviderID == providerID && b.PlanID == planID {
			out = append(out, b.Category)
		}
	}
	return out, nil
}

func (s *Store) HasBookingForParticipant(_ context.Context, providerID, participantID string) (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return slices.ContainsFunc(s.bookings, func(b domain.ServiceBooking) bool {
		plan, ok := s.plans[b.PlanID]
		return ok && b.ProviderID == providerID && plan.ParticipantID == participantID
	}), nil
}
