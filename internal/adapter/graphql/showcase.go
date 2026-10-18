package graphql

import (
	"fmt"
	"time"

	"github.com/eliteGoblin/eks-gitops-demo/internal/adapter/graphql/model"
	"github.com/eliteGoblin/eks-gitops-demo/internal/app"
	"github.com/eliteGoblin/eks-gitops-demo/internal/domain"
)

const defaultSearchSize = 10

// The fixed part of what a lookup by id or a search costs, against the
// complexity limit: node and nodes read up to one batch per record type, a
// search runs three scoped queries and writes an audit line. On top of it,
// each id asked for and each result costs its selection, so aliases cannot
// pack many lookups or searches into one cheap-looking request.
const (
	nodeLookupCost = 10
	searchCost     = 20
)

// searchSize is the number of results a search may return, for its cost.
func searchSize(first *int) int {
	if first == nil {
		return defaultSearchSize
	}
	return min(max(*first, 1), app.MaxSearchResults)
}

// nodeOf converts a record found by id to its GraphQL type; nil stays null.
func nodeOf(r *app.Record) model.Node {
	switch {
	case r == nil:
		return nil
	case r.Claim != nil:
		return &model.Claim{Claim: *r.Claim}
	case r.SupportRequest != nil:
		return &model.SupportRequest{SupportRequest: *r.SupportRequest}
	case r.Plan != nil:
		return r.Plan
	case r.Participant != nil:
		return r.Participant
	case r.Provider != nil:
		return r.Provider
	}
	return nil
}

// searchResults flattens the groups in their order: participants, providers, claims.
func searchResults(res app.SearchResults) []model.SearchResult {
	out := make([]model.SearchResult, 0, len(res.Participants)+len(res.Providers)+len(res.Claims))
	for i := range res.Participants {
		out = append(out, &res.Participants[i])
	}
	for i := range res.Providers {
		out = append(out, &res.Providers[i])
	}
	for i := range res.Claims {
		out = append(out, &model.Claim{Claim: res.Claims[i]})
	}
	return out
}

// claimFilter combines the legacy status argument with the filter input.
func claimFilter(planID string, status *domain.ClaimStatus, in *model.ClaimFilter) (app.ClaimFilter, error) {
	f := app.ClaimFilter{PlanID: planID, Status: status}
	if in == nil {
		return f, nil
	}
	if in.Status != nil {
		if status != nil && *status != *in.Status {
			return f, fmt.Errorf("%w: status and filter.status disagree", domain.ErrInvalidInput)
		}
		f.Status = in.Status
	}
	f.Category = in.Category
	f.ProviderID = deref(in.ProviderID)
	var err error
	if f.SubmittedAfter, err = timestamp("filter.submittedAfter", in.SubmittedAfter); err != nil {
		return f, err
	}
	if f.SubmittedBefore, err = timestamp("filter.submittedBefore", in.SubmittedBefore); err != nil {
		return f, err
	}
	return f, nil
}

func timestamp(name string, s *string) (*time.Time, error) {
	if s == nil {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339Nano, *s)
	if err != nil {
		return nil, fmt.Errorf("%w: %s must be an RFC 3339 timestamp", domain.ErrInvalidInput, name)
	}
	t = t.UTC()
	return &t, nil
}

func claimOrder(in *model.ClaimOrder) app.ClaimOrder {
	o := app.ClaimOrder{Field: app.OrderBySubmittedAt}
	if in == nil {
		return o
	}
	if in.Field == model.ClaimOrderFieldAmount {
		o.Field = app.OrderByAmount
	}
	o.Descending = in.Direction == model.OrderDirectionDesc
	return o
}
