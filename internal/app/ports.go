package app

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/eliteGoblin/eks-gitops-demo/internal/domain"
)

// Repositories return domain.ErrNotFound when an entity does not exist. Batch
// reads (Get...s) return the entities that exist, in any order.

// ParticipantRepository reads participants.
type ParticipantRepository interface {
	GetParticipant(ctx context.Context, id string) (domain.Participant, error)
	GetParticipants(ctx context.Context, ids []string) ([]domain.Participant, error)
	// SearchParticipants returns up to limit participants within scope whose
	// full name or member number contains term (case-insensitive, matched
	// literally: no wildcards), by family name, given name, id.
	SearchParticipants(ctx context.Context, term string, scope Scope, limit int) ([]domain.Participant, error)
}

// PlanRepository reads plans.
type PlanRepository interface {
	GetPlan(ctx context.Context, id string) (domain.Plan, error)
	GetPlans(ctx context.Context, ids []string) ([]domain.Plan, error)
	ListPlansByParticipant(ctx context.Context, participantID string) ([]domain.Plan, error)
}

// ProviderRepository reads providers.
type ProviderRepository interface {
	GetProvider(ctx context.Context, id string) (domain.Provider, error)
	GetProviders(ctx context.Context, ids []string) ([]domain.Provider, error)
	// SearchProviders returns up to limit providers whose name or ABN contains
	// term (case-insensitive, matched literally), by name, id.
	SearchProviders(ctx context.Context, term string, limit int) ([]domain.Provider, error)
}

// Scope is the set of records a caller may list, as the policy derives it from
// the principal: everything, or the records of some participants, or the
// records a provider owns. A record matches when any part matches.
type Scope struct {
	All            bool
	ParticipantIDs []string
	ProviderID     string // claims owned by this provider
	RequestedBy    string // support requests raised by this subject
}

// Empty reports whether the scope matches nothing.
func (s Scope) Empty() bool {
	return !s.All && len(s.ParticipantIDs) == 0 && s.ProviderID == "" && s.RequestedBy == ""
}

// matchesClaim reports whether a claim on participantID's plan is in scope.
func (s Scope) matchesClaim(c domain.Claim, participantID string) bool {
	return s.All || slices.Contains(s.ParticipantIDs, participantID) || (s.ProviderID != "" && c.ProviderID == s.ProviderID)
}

// ClaimOrderField is the key a claim listing is sorted by; the claim id
// breaks ties, so the order is total and keyset pagination stays exact.
type ClaimOrderField string

const (
	OrderBySubmittedAt ClaimOrderField = "SUBMITTED_AT"
	OrderByAmount      ClaimOrderField = "AMOUNT"
)

// ClaimOrder is the sort order of a claim listing. The zero value is the
// default: oldest submission first.
type ClaimOrder struct {
	Field      ClaimOrderField // empty means OrderBySubmittedAt
	Descending bool
}

// Normalized returns the order with its default field filled in.
func (o ClaimOrder) Normalized() ClaimOrder {
	if o.Field == "" {
		o.Field = OrderBySubmittedAt
	}
	return o
}

// ClaimCursor is a keyset position in one order: the sort key of the last
// claim returned (SubmittedAt or Amount, per Order.Field) and its id.
type ClaimCursor struct {
	Order       ClaimOrder
	SubmittedAt time.Time
	Amount      domain.Money
	ID          string
}

// ClaimQuery selects claims within a scope, sorted by Order.
type ClaimQuery struct {
	Scope    Scope
	Filter   ClaimFilter
	IDPrefix string // only claims whose id starts with it (search); empty = any
	Order    ClaimOrder
	After    *ClaimCursor // exclusive; nil = from the start
	Limit    int          // 0 = no limit
}

// ClaimRepository stores claims.
type ClaimRepository interface {
	GetClaim(ctx context.Context, id string) (domain.Claim, error)
	GetClaims(ctx context.Context, ids []string) ([]domain.Claim, error)
	// ClaimsOfPlan returns every claim of a plan (budget arithmetic needs all of them).
	ClaimsOfPlan(ctx context.Context, planID string) ([]domain.Claim, error)
	// QueryClaims returns one page of the claims a query selects.
	QueryClaims(ctx context.Context, q ClaimQuery) ([]domain.Claim, error)
	// CountClaims counts what QueryClaims would select, ignoring After and Limit.
	CountClaims(ctx context.Context, q ClaimQuery) (int, error)
	// InsertClaimWithinBudget stores c only if check accepts the current claims
	// and bookings of c's plan, as one atomic step: no other budget write for
	// that plan may run between the check and the write. check returns the
	// business error to surface (e.g. *domain.BudgetExceededError). The SQL
	// adapter runs it as one transaction that locks the plan row first.
	InsertClaimWithinBudget(ctx context.Context, c domain.Claim, check BudgetCheck) error
	// InsertClaimOnce is InsertClaimWithinBudget for a request that carries
	// an idempotency key: in the same atomic step it records the key against
	// c's id. When the caller already used the key it stores nothing and
	// returns the earlier record with replayed true. Concurrent requests with
	// one key store at most one claim.
	InsertClaimOnce(ctx context.Context, c domain.Claim, key IdempotencyKey, check BudgetCheck) (prior IdempotencyRecord, replayed bool, err error)
	// UpdateClaim stores c's status and payment as one conditional write, so
	// two concurrent transitions cannot both win: it applies only while the
	// stored status is still from, returns domain.ClaimNotInStatus(from) when
	// it is not, and domain.ErrNotFound when the claim does not exist.
	UpdateClaim(ctx context.Context, c domain.Claim, from domain.ClaimStatus) error
}

// IdempotencyKey identifies a retryable request: Key is the client's, scoped
// to the caller (Subject); Fingerprint identifies the request's content.
type IdempotencyKey struct {
	Subject     string
	Key         string
	Fingerprint string
}

// IdempotencyRecord is what a used idempotency key points at.
type IdempotencyRecord struct {
	ClaimID     string
	Fingerprint string
}

// BudgetCheck inspects the claims and bookings already recorded on a plan and
// returns an error when a new claim must not be stored.
type BudgetCheck func(planClaims []domain.Claim, bookings []domain.ServiceBooking) error

// BookingRepository reads service bookings.
type BookingRepository interface {
	BookingsOfPlan(ctx context.Context, planID string) ([]domain.ServiceBooking, error)
}

// SupportRequestQuery selects support requests within a scope, oldest first.
type SupportRequestQuery struct {
	Scope  Scope
	Status *domain.SupportRequestStatus
}

// SupportRequestRepository stores support requests.
type SupportRequestRepository interface {
	InsertSupportRequest(ctx context.Context, r domain.SupportRequest) error
	GetSupportRequest(ctx context.Context, id string) (domain.SupportRequest, error)
	GetSupportRequests(ctx context.Context, ids []string) ([]domain.SupportRequest, error)
	ListSupportRequests(ctx context.Context, q SupportRequestQuery) ([]domain.SupportRequest, error)
	// DecideSupportRequest stores a decided request, but only if it is still
	// PENDING (otherwise a *domain.ValidationError on "id"). For an approval
	// it also adds r.EstimatedAmount to the booking of r.BookedProviderID,
	// after check accepts the plan's current claims and bookings, all in one
	// atomic step (the SQL adapter locks the plan row first). check is nil
	// for a rejection.
	DecideSupportRequest(ctx context.Context, r domain.SupportRequest, reserve Reservation) error
}

// Reservation decides, from a plan's claims and bookings read under the plan
// lock, the provider's booking after a new reservation (which the store then
// saves as is), or returns the business error that refuses it.
type Reservation func(planClaims []domain.Claim, bookings []domain.ServiceBooking) (domain.ServiceBooking, error)

// ErrUnavailable marks a transient storage failure: a lock or statement wait
// that ran out, a session cut off, a serialization conflict. The request was
// not wrong and may succeed if retried; repositories wrap such failures with it.
var ErrUnavailable = errors.New("temporarily unavailable")

// Relationships answers the questions the policy asks about links between
// callers and records.
type Relationships interface {
	IsActiveDelegate(ctx context.Context, nomineeID, participantID string) (bool, error)
	DelegatedParticipants(ctx context.Context, nomineeID string) ([]string, error)
	BookedCategories(ctx context.Context, providerID, planID string) ([]domain.Category, error)
	HasBookingForParticipant(ctx context.Context, providerID, participantID string) (bool, error)
	// BookedParticipants lists the participants a provider holds any booking for.
	BookedParticipants(ctx context.Context, providerID string) ([]string, error)
}

// AuditEvent is one access decision or state change. A decision made before
// the record is read (allow or deny of an approval, a decision, a refusal at
// the role gate) names the record as the caller sent it, so the id may not
// exist and the plan and parties are empty; the outcome events that follow
// name the record as stored.
type AuditEvent struct {
	Actor    string
	Username string
	Roles    []Role
	Action   Action
	Resource Resource
	Outcome  string // "allow", "deny", "error" or a state-change verb
	Detail   string
}

// Auditor records audit events; the adapter decides where they go.
type Auditor interface {
	Record(ctx context.Context, e AuditEvent)
}

// Clock returns the current time; tests pass a fixed one.
type Clock func() time.Time

// IDGenerator returns a new unique identifier.
type IDGenerator func() string
