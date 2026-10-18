package memory

import (
	"cmp"
	"context"
	"slices"
	"strings"

	"github.com/eliteGoblin/eks-gitops-demo/internal/app"
	"github.com/eliteGoblin/eks-gitops-demo/internal/domain"
)

// QueryClaims applies the scope, filter, order and keyset page of q.
func (s *Store) QueryClaims(_ context.Context, q app.ClaimQuery) ([]domain.Claim, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	order := q.Order.Normalized()
	out := []domain.Claim{}
	for _, c := range s.selectClaims(q) {
		if q.After != nil && !sortsAfter(c, *q.After, order) {
			continue
		}
		out = append(out, c)
		if q.Limit > 0 && len(out) == q.Limit {
			break
		}
	}
	return out, nil
}

// CountClaims counts what q selects, ignoring its page.
func (s *Store) CountClaims(_ context.Context, q app.ClaimQuery) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.selectClaims(q)), nil
}

// selectClaims returns the claims in q's scope that pass its filter, in q's
// order. The caller holds s.mu.
func (s *Store) selectClaims(q app.ClaimQuery) []domain.Claim {
	out := []domain.Claim{}
	for _, c := range s.claimsOfPlan(q.Filter.PlanID) {
		if matchesFilter(c, q.Filter) && hasPrefixFold(c.ID, q.IDPrefix) && s.inScope(q.Scope, c) {
			out = append(out, c)
		}
	}
	order := q.Order.Normalized()
	slices.SortFunc(out, func(a, b domain.Claim) int { return compareClaims(a, b, order) })
	return out
}

func matchesFilter(c domain.Claim, f app.ClaimFilter) bool {
	switch {
	case f.Status != nil && c.Status != *f.Status,
		f.Category != nil && c.Category != *f.Category,
		f.ProviderID != "" && c.ProviderID != f.ProviderID,
		f.SubmittedAfter != nil && c.SubmittedAt.Before(*f.SubmittedAfter),
		f.SubmittedBefore != nil && !c.SubmittedAt.Before(*f.SubmittedBefore):
		return false
	}
	return true
}

// compareClaims orders by the order's key, then by id (byte order, as the
// SQL adapter's COLLATE "C"), reversed for a descending order.
func compareClaims(a, b domain.Claim, o app.ClaimOrder) int {
	var c int
	if o.Field == app.OrderByAmount {
		c = cmp.Compare(a.Amount, b.Amount)
	} else {
		c = a.SubmittedAt.Compare(b.SubmittedAt)
	}
	if c == 0 {
		c = strings.Compare(a.ID, b.ID)
	}
	if o.Descending {
		return -c
	}
	return c
}

// sortsAfter reports whether c comes strictly after the cursor in order o.
func sortsAfter(c domain.Claim, cur app.ClaimCursor, o app.ClaimOrder) bool {
	pivot := domain.Claim{ID: cur.ID, SubmittedAt: cur.SubmittedAt, Amount: cur.Amount}
	return compareClaims(c, pivot, o) > 0
}

// InsertClaimOnce checks the idempotency key and the budget and inserts under
// one write lock, so concurrent requests with one key store one claim.
func (s *Store) InsertClaimOnce(_ context.Context, c domain.Claim, k app.IdempotencyKey, check app.BudgetCheck) (app.IdempotencyRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := idempotencyID{subject: k.Subject, key: k.Key}
	if prior, ok := s.idempotency[id]; ok {
		return prior, true, nil
	}
	if err := check(s.claimsOfPlan(c.PlanID), s.bookingsOfPlan(c.PlanID)); err != nil {
		return app.IdempotencyRecord{}, false, err
	}
	s.claims[c.ID] = cloneClaim(c)
	s.idempotency[id] = app.IdempotencyRecord{ClaimID: c.ID, Fingerprint: k.Fingerprint}
	return app.IdempotencyRecord{}, false, nil
}

// SearchParticipants matches the full name or member number, case-insensitively.
func (s *Store) SearchParticipants(_ context.Context, term string, scope app.Scope, limit int) ([]domain.Participant, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []domain.Participant{}
	for _, p := range s.participants {
		inScope := scope.All || slices.Contains(scope.ParticipantIDs, p.ID)
		if inScope && (containsFold(p.GivenName+" "+p.FamilyName, term) || containsFold(p.SchemeNumber, term)) {
			out = append(out, cloneParticipant(p))
		}
	}
	slices.SortFunc(out, func(a, b domain.Participant) int {
		return cmp.Or(strings.Compare(a.FamilyName, b.FamilyName), strings.Compare(a.GivenName, b.GivenName), strings.Compare(a.ID, b.ID))
	})
	return firstN(out, limit), nil
}

// SearchProviders matches the name or ABN, case-insensitively.
func (s *Store) SearchProviders(_ context.Context, term string, limit int) ([]domain.Provider, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []domain.Provider{}
	for _, p := range s.providers {
		if containsFold(p.Name, term) || containsFold(p.ABN, term) {
			out = append(out, p)
		}
	}
	slices.SortFunc(out, func(a, b domain.Provider) int {
		return cmp.Or(strings.Compare(a.Name, b.Name), strings.Compare(a.ID, b.ID))
	})
	return firstN(out, limit), nil
}

// BookedParticipants lists, sorted, the participants whose plans the provider
// holds a booking on.
func (s *Store) BookedParticipants(_ context.Context, providerID string) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []string{}
	for _, b := range s.bookings {
		plan, ok := s.plans[b.PlanID]
		if ok && b.ProviderID == providerID && !slices.Contains(out, plan.ParticipantID) {
			out = append(out, plan.ParticipantID)
		}
	}
	slices.Sort(out)
	return out, nil
}

func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}

func hasPrefixFold(s, prefix string) bool {
	return strings.HasPrefix(strings.ToLower(s), strings.ToLower(prefix))
}

func firstN[T any](items []T, n int) []T {
	if n > 0 && len(items) > n {
		return items[:n]
	}
	return items
}
