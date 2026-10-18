package app

import (
	"fmt"
	"slices"
)

// Deps are the ports the use cases need. Every field is required except
// Payments (nil turns paying approved claims off) and Events (nil announces
// nothing and makes Watch return ErrEventsUnavailable).
type Deps struct {
	Participants    ParticipantRepository
	Plans           PlanRepository
	Providers       ProviderRepository
	Claims          ClaimRepository
	Bookings        BookingRepository
	SupportRequests SupportRequestRepository
	Relationships   Relationships
	Auditor         Auditor
	Clock           Clock
	NextID          IDGenerator
	Payments        PaymentGateway
	Events          Events
}

func (d Deps) validate() error {
	var missing []string
	for name, isNil := range map[string]bool{
		"Participants": d.Participants == nil, "Plans": d.Plans == nil, "Providers": d.Providers == nil,
		"Claims": d.Claims == nil, "Bookings": d.Bookings == nil, "SupportRequests": d.SupportRequests == nil,
		"Relationships": d.Relationships == nil, "Auditor": d.Auditor == nil,
		"Clock": d.Clock == nil, "NextID": d.NextID == nil,
	} {
		if isNil {
			missing = append(missing, name)
		}
	}
	slices.Sort(missing)
	if len(missing) > 0 {
		return fmt.Errorf("app: missing dependencies %v", missing)
	}
	return nil
}

// Services groups the use cases the transport layer calls.
type Services struct {
	Participants    *ParticipantService
	Plans           *PlanService
	Providers       *ProviderService
	Claims          *ClaimService
	SupportRequests *SupportRequestService
	Nodes           *NodeService
	Search          *SearchService
	RoleGate        RoleGate
}

// NewServices wires the use cases. Authorization and audit run inside every
// use case, so every transport path (including nested GraphQL fields) pays them.
func NewServices(d Deps) (*Services, error) {
	if err := d.validate(); err != nil {
		return nil, err
	}
	g := guard{policy: NewPolicy(d.Relationships), audit: d.Auditor}
	events := d.Events
	if events == nil {
		events = noEvents{}
	}
	plans := planAccess{plans: d.Plans, guard: g}
	participants := &ParticipantService{repo: d.Participants, guard: g}
	svc := &Services{
		Participants: participants,
		Plans:        &PlanService{plans: d.Plans, claims: d.Claims, bookings: d.Bookings, guard: g},
		Providers:    &ProviderService{repo: d.Providers},
		Claims: &ClaimService{claims: d.Claims, plans: d.Plans, planAccess: plans, guard: g, now: d.Clock, nextID: d.NextID,
			payments: d.Payments, events: events},
		SupportRequests: &SupportRequestService{requests: d.SupportRequests, plans: d.Plans, providers: d.Providers,
			planAccess: plans, guard: g, now: d.Clock, nextID: d.NextID, events: events},
		Search:   &SearchService{participants: d.Participants, providers: d.Providers, claims: d.Claims, people: participants, guard: g},
		RoleGate: RoleGate{guard: g},
	}
	svc.Nodes = newNodeService(d, svc)
	return svc, nil
}
