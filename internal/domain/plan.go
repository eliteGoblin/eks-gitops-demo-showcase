package domain

// Allocation returns the allocation for a category, if the plan has one.
func (p Plan) Allocation(c Category) (Allocation, bool) {
	for _, a := range p.Allocations {
		if a.Category == c {
			return a, true
		}
	}
	return Allocation{}, false
}

// Budgets derives the budget of every allocation from the plan's claims and
// service bookings.
func (p Plan) Budgets(claims []Claim, bookings []ServiceBooking) []Budget {
	out := make([]Budget, 0, len(p.Allocations))
	for _, a := range p.Allocations {
		spent := SpentIn(claims, a.Category)
		out = append(out, Budget{
			Category: a.Category, Allocated: a.Allocated, Spent: spent,
			Reserved: OutstandingReserved(claims, bookings, a.Category), Remaining: a.Allocated - spent,
		})
	}
	return out
}

// CheckClaim applies the claiming rules for providerID: the plan must be
// active, the service must fall inside the plan period, and the category
// needs an allocation with enough budget. Budget is what is neither spent
// nor reserved for another provider; the provider's own outstanding
// reservation stays available to it. It returns a *ValidationError or
// *BudgetExceededError when a rule fails.
func (p Plan) CheckClaim(providerID string, c Category, amount Money, serviceDate Date, claims []Claim, bookings []ServiceBooking) error {
	alloc, err := p.activeAllocation(c)
	if err != nil {
		return err
	}
	if serviceDate < p.StartDate || serviceDate > p.EndDate {
		return &ValidationError{Field: "serviceDate", Message: "service date is outside the plan period"}
	}
	remaining := max(0, alloc.Allocated-SpentIn(claims, c)-outstandingReserved(claims, bookings, c, providerID))
	if amount > remaining {
		return &BudgetExceededError{Remaining: remaining, Requested: amount}
	}
	return nil
}

// CheckReservation applies the booking rule: a new reservation must fit in
// what is neither spent nor already reserved for another booking.
func (p Plan) CheckReservation(c Category, amount Money, claims []Claim, bookings []ServiceBooking) error {
	alloc, err := p.activeAllocation(c)
	if err != nil {
		return err
	}
	available := max(0, alloc.Allocated-SpentIn(claims, c)-OutstandingReserved(claims, bookings, c))
	if amount > available {
		return &BudgetExceededError{Remaining: available, Requested: amount}
	}
	return nil
}

// Reserve applies the booking rule and returns providerID's booking after
// reserving amount on category c. A booking is cumulative (one row per
// provider, plan and category) and a provider's claims consume it, so the new
// reservation is added on top of what the provider has already consumed:
// max(reserved so far, claimed so far) + amount. That holds the whole amount
// for the provider even when it claimed beyond an earlier reservation, which
// would otherwise absorb part of the new one.
func (p Plan) Reserve(providerID string, c Category, amount Money, claims []Claim, bookings []ServiceBooking) (ServiceBooking, error) {
	if err := p.CheckReservation(c, amount, claims, bookings); err != nil {
		return ServiceBooking{}, err
	}
	consumed := max(reservedFor(bookings, providerID, c), spentBy(claims, providerID, c))
	return ServiceBooking{ProviderID: providerID, PlanID: p.ID, Category: c, Reserved: consumed + amount}, nil
}

// reservedFor is what providerID has reserved on category c so far.
func reservedFor(bookings []ServiceBooking, providerID string, c Category) Money {
	var total Money
	for _, b := range bookings {
		if b.ProviderID == providerID && b.Category == c {
			total += b.Reserved
		}
	}
	return total
}

// activeAllocation returns the category's allocation of an active plan.
func (p Plan) activeAllocation(c Category) (Allocation, error) {
	if p.Status != PlanActive {
		return Allocation{}, &ValidationError{Field: "planId", Message: "plan is not active"}
	}
	alloc, ok := p.Allocation(c)
	if !ok {
		return Allocation{}, &ValidationError{Field: "category", Message: "plan has no budget for this category"}
	}
	return alloc, nil
}

// OutstandingReserved is the part of the category's bookings that their
// providers have not claimed yet. A provider's claims consume its own
// reservation first; claiming more than was reserved frees nothing else.
func OutstandingReserved(claims []Claim, bookings []ServiceBooking, c Category) Money {
	return outstandingReserved(claims, bookings, c, "")
}

// outstandingReserved is OutstandingReserved without the bookings of
// exceptProvider (whose own reservation is not held against it).
func outstandingReserved(claims []Claim, bookings []ServiceBooking, c Category, exceptProvider string) Money {
	var total Money
	for _, b := range bookings {
		if b.Category != c || b.ProviderID == exceptProvider {
			continue
		}
		claimed := spentBy(claims, b.ProviderID, c)
		total += max(0, b.Reserved-claimed)
	}
	return total
}

func spentBy(claims []Claim, providerID string, c Category) Money {
	var total Money
	for _, cl := range claims {
		if cl.ProviderID == providerID && cl.Category == c && cl.Status != ClaimRejected {
			total += cl.Amount
		}
	}
	return total
}

// SpentIn sums the claims that still consume budget (everything not rejected).
func SpentIn(claims []Claim, c Category) Money {
	var total Money
	for _, cl := range claims {
		if cl.Category == c && cl.Status != ClaimRejected {
			total += cl.Amount
		}
	}
	return total
}
