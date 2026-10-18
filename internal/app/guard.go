package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/eliteGoblin/eks-gitops-demo/internal/domain"
)

// guard runs the policy and audits every decision.
type guard struct {
	policy *Policy
	audit  Auditor
}

func (g guard) authorize(ctx context.Context, a Action, res Resource) (Principal, error) {
	pr, ok := PrincipalFrom(ctx)
	if !ok {
		return pr, ErrUnauthenticated
	}
	err := g.policy.Can(ctx, pr, a, res)
	g.audit.Record(ctx, AuditEvent{Actor: pr.Subject, Username: pr.Username, Roles: pr.RoleNames(), Action: a, Resource: res, Outcome: outcomeOf(err)})
	return pr, err
}

// principal returns the caller or ErrUnauthenticated.
func (g guard) principal(ctx context.Context) (Principal, error) {
	pr, ok := PrincipalFrom(ctx)
	if !ok {
		return pr, ErrUnauthenticated
	}
	return pr, nil
}

// recordList audits a listing once, with how much of what matched was returned.
func (g guard) recordList(ctx context.Context, pr Principal, a Action, res Resource, scope Scope, returned, matched int, noun string) {
	outcome := "allow"
	if scope.Empty() {
		outcome = "deny"
	}
	g.audit.Record(ctx, AuditEvent{Actor: pr.Subject, Username: pr.Username, Roles: pr.RoleNames(), Action: a, Resource: res,
		Outcome: outcome, Detail: fmt.Sprintf("%d of %d %s", returned, matched, noun)})
}

// recordChange audits a state change with its reason.
func (g guard) recordChange(ctx context.Context, pr Principal, a Action, res Resource, verb, reason string) {
	g.audit.Record(ctx, AuditEvent{Actor: pr.Subject, Username: pr.Username, Roles: pr.RoleNames(), Action: a, Resource: res, Outcome: verb, Detail: reason})
}

// RoleGate is the coarse role check a transport runs before it calls a use
// case (the GraphQL @auth directive). It audits what it refuses: the use case
// behind a refused request never runs, so it cannot put the attempt on
// record. An allowed request is audited by the use case it reaches.
type RoleGate struct{ guard guard }

// Require returns nil when the caller holds any of roles. Otherwise it
// records a deny for a on res, naming the required roles as the reason, and
// returns ErrForbidden. Without a caller it returns ErrUnauthenticated.
func (g RoleGate) Require(ctx context.Context, a Action, res Resource, roles ...Role) error {
	pr, err := g.guard.principal(ctx)
	if err != nil {
		return err
	}
	if slices.ContainsFunc(roles, pr.HasRole) {
		return nil
	}
	names := make([]string, len(roles))
	for i, r := range roles {
		names[i] = string(r)
	}
	g.guard.audit.Record(ctx, AuditEvent{Actor: pr.Subject, Username: pr.Username, Roles: pr.RoleNames(), Action: a, Resource: clipIDs(res),
		Outcome: "deny", Detail: "requires role " + strings.Join(names, " or ")})
	return ErrForbidden
}

// maxRefusedID bounds each id in a refusal's audit line. Real ids have at
// most 36 characters; a caller who was just refused must not be able to write
// a request body's worth of text into the audit trail.
const maxRefusedID = 64

// clipIDs returns res with every id longer than maxRefusedID bytes cut to it
// and marked with "…".
func clipIDs(res Resource) Resource {
	for _, id := range []*string{&res.ParticipantID, &res.PlanID, &res.ProviderID, &res.RequestedBy, &res.ClaimID, &res.SupportRequestID} {
		if len(*id) > maxRefusedID {
			*id = strings.ToValidUTF8((*id)[:maxRefusedID], "") + "…"
		}
	}
	return res
}

// outcomeOf separates a policy denial from a failure to decide (e.g. a lookup
// error), so the audit trail never records an outage as a denial.
func outcomeOf(err error) string {
	switch {
	case err == nil:
		return "allow"
	case errors.Is(err, ErrForbidden):
		return "deny"
	default:
		return "error"
	}
}

// hideDenied turns "forbidden" and "not found" into (nil, nil): the client
// cannot tell them apart, so personal records cannot be enumerated.
func hideDenied(err error) error {
	if errors.Is(err, ErrForbidden) || errors.Is(err, domain.ErrNotFound) {
		return nil
	}
	return err
}
