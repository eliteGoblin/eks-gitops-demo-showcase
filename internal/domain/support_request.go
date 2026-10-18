package domain

import (
	"strings"
	"time"
)

// SupportRequestStatus is where a support request is in its life cycle.
type SupportRequestStatus string

const (
	SupportRequestPending  SupportRequestStatus = "PENDING"
	SupportRequestApproved SupportRequestStatus = "APPROVED"
	SupportRequestRejected SupportRequestStatus = "REJECTED"
)

const maxDescriptionLength = 500

// SupportRequest asks the agency to fund a support from a plan. Approving it
// books a provider and reserves the estimated amount of the category budget;
// the provider then claims against that booking.
type SupportRequest struct {
	ID                  string
	PlanID              string
	ParticipantID       string
	RequestedBy         string // subject of the requester
	RequestedByName     string
	Category            Category
	Description         string
	EstimatedAmount     Money
	PreferredProviderID string // optional
	Status              SupportRequestStatus
	DecidedBy           string
	DecisionReason      string
	BookedProviderID    string
	RaisedAt            time.Time
	DecidedAt           *time.Time
}

// SupportRequestDraft is what a requester supplies; the plan is looked up first.
type SupportRequestDraft struct {
	ID                  string
	Plan                Plan
	RequestedBy         string
	RequestedByName     string
	Category            Category
	Description         string
	EstimatedAmount     Money
	PreferredProviderID string
}

// NewSupportRequest validates a draft and returns a PENDING request.
func NewSupportRequest(d SupportRequestDraft, now time.Time) (SupportRequest, error) {
	if _, err := d.Plan.activeAllocation(d.Category); err != nil {
		return SupportRequest{}, err
	}
	if d.EstimatedAmount <= 0 {
		return SupportRequest{}, &ValidationError{Field: "estimatedAmount", Message: "estimated amount must be greater than zero"}
	}
	desc := strings.TrimSpace(d.Description)
	if desc == "" || len(desc) > maxDescriptionLength {
		return SupportRequest{}, &ValidationError{Field: "description", Message: "description must be 1 to 500 characters"}
	}
	return SupportRequest{
		ID: d.ID, PlanID: d.Plan.ID, ParticipantID: d.Plan.ParticipantID,
		RequestedBy: d.RequestedBy, RequestedByName: d.RequestedByName,
		Category: d.Category, Description: desc, EstimatedAmount: d.EstimatedAmount,
		PreferredProviderID: d.PreferredProviderID, Status: SupportRequestPending, RaisedAt: now.UTC(),
	}, nil
}

// Approve books providerID (or the preferred provider when it is empty).
// Nobody may decide a request they raised.
func (r *SupportRequest) Approve(decidedBy, providerID, reason string, now time.Time) error {
	if err := r.checkDecision(decidedBy, reason); err != nil {
		return err
	}
	if providerID == "" {
		providerID = r.PreferredProviderID
	}
	if providerID == "" {
		return &ValidationError{Field: "providerId", Message: "choose a provider to book"}
	}
	r.BookedProviderID = providerID
	r.decide(SupportRequestApproved, decidedBy, reason, now)
	return nil
}

// Reject closes the request without booking anything.
func (r *SupportRequest) Reject(decidedBy, reason string, now time.Time) error {
	if err := r.checkDecision(decidedBy, reason); err != nil {
		return err
	}
	r.decide(SupportRequestRejected, decidedBy, reason, now)
	return nil
}

func (r *SupportRequest) checkDecision(decidedBy, reason string) error {
	if r.Status != SupportRequestPending {
		return &ValidationError{Field: "id", Message: "support request is already decided"}
	}
	if decidedBy == r.RequestedBy {
		return &SelfApprovalNotAllowedError{}
	}
	if strings.TrimSpace(reason) == "" {
		return &ValidationError{Field: "reason", Message: "reason is required"}
	}
	return nil
}

func (r *SupportRequest) decide(s SupportRequestStatus, decidedBy, reason string, now time.Time) {
	at := now.UTC()
	r.Status, r.DecidedBy, r.DecisionReason, r.DecidedAt = s, decidedBy, strings.TrimSpace(reason), &at
}
