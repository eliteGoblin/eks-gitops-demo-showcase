package app

import (
	"context"
	"slices"

	"github.com/eliteGoblin/eks-gitops-demo/internal/domain"
)

// Action names what the caller wants to do.
type Action string

const (
	ActionReadParticipant Action = "participant:read"
	ActionReadPlan        Action = "plan:read"
	ActionReadClaim       Action = "claim:read"
	ActionSubmitClaim     Action = "claim:submit"
	ActionApproveClaim    Action = "claim:approve"

	ActionRaiseSupportRequest  Action = "support_request:raise"
	ActionDecideSupportRequest Action = "support_request:decide"
	ActionReadSupportRequests  Action = "support_request:read"
	ActionReadSupportRequest   Action = "support_request:read_one"
	ActionReadSupportQueue     Action = "support_request:read_queue"

	ActionSearch Action = "search" // scoped by the policy; audited once per search
)

// Resource identifies the record an action touches. Fill the fields that apply.
type Resource struct {
	ParticipantID string
	PlanID        string
	ProviderID    string // provider that owns a claim
	Category      domain.Category
	RequestedBy   string // subject who raised a support request

	// The record itself, for the audit trail (the policy decides on the
	// relationships above, never on an id).
	ClaimID          string
	SupportRequestID string
}

// Policy decides relationship-based access: own records, active delegation,
// or a service booking. Agency staff may read everything (audited), approve
// claims and decide support requests, but never submit claims. Unknown
// actions are denied.
type Policy struct{ rel Relationships }

// NewPolicy returns a policy that looks relationships up through rel.
func NewPolicy(rel Relationships) *Policy { return &Policy{rel: rel} }

// Can returns nil when pr may perform a on res, otherwise ErrForbidden.
func (p *Policy) Can(ctx context.Context, pr Principal, a Action, res Resource) error {
	switch a {
	case ActionApproveClaim, ActionDecideSupportRequest, ActionReadSupportQueue:
		return staffOnly(pr)
	case ActionRaiseSupportRequest:
		return p.raise(ctx, pr, res)
	case ActionSubmitClaim:
		return p.providerSubmit(ctx, pr, res)
	case ActionReadParticipant, ActionReadPlan, ActionReadClaim:
		return p.read(ctx, pr, a, res)
	case ActionReadSupportRequest:
		return p.readSupportRequest(ctx, pr, res)
	default:
		return ErrForbidden
	}
}

// readSupportRequest follows mySupportRequests: the requester and the people
// the request is about (the participant or an active nominee) may read it;
// staff, who work the queue, may read any.
func (p *Policy) readSupportRequest(ctx context.Context, pr Principal, res Resource) error {
	if pr.HasRole(RoleAgencyStaff) || (res.RequestedBy != "" && res.RequestedBy == pr.Subject) {
		return nil
	}
	ok, err := p.canReadAsPerson(ctx, pr, res.ParticipantID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrForbidden
	}
	return nil
}

func staffOnly(pr Principal) error {
	if pr.HasRole(RoleAgencyStaff) {
		return nil
	}
	return ErrForbidden
}

func (p *Policy) read(ctx context.Context, pr Principal, a Action, res Resource) error {
	if pr.HasRole(RoleAgencyStaff) {
		return nil
	}
	if ok, err := p.canReadAsPerson(ctx, pr, res.ParticipantID); err != nil || ok {
		return err
	}
	if pr.HasRole(RoleProvider) && pr.ProviderID != "" {
		return p.providerRead(ctx, pr, a, res)
	}
	return ErrForbidden
}

// raise lets a participant or their active nominee ask for support on their
// own plans; staff may lodge a request for anyone.
func (p *Policy) raise(ctx context.Context, pr Principal, res Resource) error {
	if pr.HasRole(RoleAgencyStaff) {
		return nil
	}
	ok, err := p.canReadAsPerson(ctx, pr, res.ParticipantID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrForbidden
	}
	return nil
}

// ClaimScope is the set of claims the principal may list: everything for
// staff, the claims on the plans of the participants it is or acts for, and
// the claims its provider submitted.
func (p *Policy) ClaimScope(ctx context.Context, pr Principal) (Scope, error) {
	if pr.HasRole(RoleAgencyStaff) {
		return Scope{All: true}, nil
	}
	people, err := p.peopleOf(ctx, pr)
	if err != nil {
		return Scope{}, err
	}
	s := Scope{ParticipantIDs: people}
	if pr.HasRole(RoleProvider) {
		s.ProviderID = pr.ProviderID
	}
	return s, nil
}

// ParticipantScope is the participant read rule as a set: every participant
// for staff, the ones the principal is or acts for, and for a provider the
// ones it holds a booking for.
func (p *Policy) ParticipantScope(ctx context.Context, pr Principal) (Scope, error) {
	if pr.HasRole(RoleAgencyStaff) {
		return Scope{All: true}, nil
	}
	people, err := p.peopleOf(ctx, pr)
	if err != nil {
		return Scope{}, err
	}
	if pr.HasRole(RoleProvider) && pr.ProviderID != "" {
		booked, err := p.rel.BookedParticipants(ctx, pr.ProviderID)
		if err != nil {
			return Scope{}, err
		}
		people = append(people, booked...)
	}
	return Scope{ParticipantIDs: people}, nil
}

// SupportRequestScope is the principal's own view of support requests: the
// ones it raised and the ones about the participants it is or acts for.
// Staff work the pending queue separately, so this is not "everything".
func (p *Policy) SupportRequestScope(ctx context.Context, pr Principal) (Scope, error) {
	people, err := p.peopleOf(ctx, pr)
	if err != nil {
		return Scope{}, err
	}
	return Scope{ParticipantIDs: people, RequestedBy: pr.Subject}, nil
}

// peopleOf lists the participants the principal is, or acts for as a nominee.
func (p *Policy) peopleOf(ctx context.Context, pr Principal) ([]string, error) {
	var ids []string
	if pr.HasRole(RoleParticipant) && pr.ParticipantID != "" {
		ids = append(ids, pr.ParticipantID)
	}
	if pr.HasRole(RoleNominee) && pr.NomineeID != "" {
		delegated, err := p.rel.DelegatedParticipants(ctx, pr.NomineeID)
		if err != nil {
			return nil, err
		}
		ids = append(ids, delegated...)
	}
	return ids, nil
}

// CanSeeDateOfBirth is the field-masking rule: the participant, an active
// nominee and agency staff see it; a provider never does.
func (p *Policy) CanSeeDateOfBirth(ctx context.Context, pr Principal, participantID string) (bool, error) {
	if pr.HasRole(RoleAgencyStaff) {
		return true, nil
	}
	return p.canReadAsPerson(ctx, pr, participantID)
}

// CanSeeBudget reports whether the caller may see one budget category of a
// plan it can already read. Providers see only their booked categories.
func (p *Policy) CanSeeBudget(ctx context.Context, pr Principal, res Resource) (bool, error) {
	if pr.HasRole(RoleAgencyStaff) {
		return true, nil
	}
	if ok, err := p.canReadAsPerson(ctx, pr, res.ParticipantID); err != nil || ok {
		return ok, err
	}
	if pr.HasRole(RoleProvider) && pr.ProviderID != "" {
		return p.hasBookedCategory(ctx, pr.ProviderID, res.PlanID, res.Category)
	}
	return false, nil
}

func (p *Policy) canReadAsPerson(ctx context.Context, pr Principal, participantID string) (bool, error) {
	if participantID == "" {
		return false, nil
	}
	if pr.HasRole(RoleParticipant) && pr.ParticipantID == participantID {
		return true, nil
	}
	if pr.HasRole(RoleNominee) && pr.NomineeID != "" {
		return p.rel.IsActiveDelegate(ctx, pr.NomineeID, participantID)
	}
	return false, nil
}

func (p *Policy) providerRead(ctx context.Context, pr Principal, a Action, res Resource) error {
	var ok bool
	var err error
	switch a {
	case ActionReadParticipant:
		ok, err = p.rel.HasBookingForParticipant(ctx, pr.ProviderID, res.ParticipantID)
	case ActionReadPlan:
		var cats []domain.Category
		cats, err = p.rel.BookedCategories(ctx, pr.ProviderID, res.PlanID)
		ok = len(cats) > 0
	case ActionReadClaim:
		ok = res.ProviderID == pr.ProviderID
	}
	if err != nil {
		return err
	}
	if !ok {
		return ErrForbidden
	}
	return nil
}

func (p *Policy) providerSubmit(ctx context.Context, pr Principal, res Resource) error {
	if !pr.HasRole(RoleProvider) || pr.ProviderID == "" {
		return ErrForbidden
	}
	ok, err := p.hasBookedCategory(ctx, pr.ProviderID, res.PlanID, res.Category)
	if err != nil {
		return err
	}
	if !ok {
		return ErrForbidden
	}
	return nil
}

func (p *Policy) hasBookedCategory(ctx context.Context, providerID, planID string, c domain.Category) (bool, error) {
	cats, err := p.rel.BookedCategories(ctx, providerID, planID)
	if err != nil {
		return false, err
	}
	return slices.Contains(cats, c), nil
}
