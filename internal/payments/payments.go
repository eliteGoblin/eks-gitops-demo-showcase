// Package payments is the core of payments-svc: the Payment entity, its
// rules and the use cases, plus the ports its adapters implement. It depends
// on nothing but the standard library; the store and the HTTP API depend on it.
package payments

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"
)

// Status is a payment's lifecycle state. The mock processor accepts payments
// synchronously; a real one would move them on to SETTLED or FAILED.
type Status string

// StatusAccepted means the payment was recorded and will be processed.
const StatusAccepted Status = "ACCEPTED"

// SupportedCurrency is the only currency the scheme pays in.
const SupportedCurrency = "AUD"

// MaxAmountCents caps a single payment (AUD 10,000,000.00).
const MaxAmountCents int64 = 1_000_000_000

const maxFieldLen = 64

// Request is what a caller asks to pay.
type Request struct {
	Reference   string // the caller's reference, e.g. the approved claim id
	Payee       string // who is paid, e.g. the provider id
	AmountCents int64
	Currency    string
}

// Payment is a recorded payment.
type Payment struct {
	ID string
	Request
	Status    Status
	Owner     string // the client that created it; only it may read it back
	CreatedAt time.Time
}

// ValidationError reports a request field the caller must fix.
type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string { return e.Field + ": " + e.Message }

var (
	// ErrForbidden means the caller lacks the permission for the operation.
	ErrForbidden = errors.New("forbidden")
	// ErrNotFound means there is no payment the caller may see with that id.
	ErrNotFound = errors.New("payment not found")
	// ErrKeyReused means an idempotency key was reused with a different request.
	ErrKeyReused = errors.New("idempotency key already used for a different request")
	// ErrStoreFull means the store cannot take another payment right now; the
	// caller may retry later with the same idempotency key.
	ErrStoreFull = errors.New("payment store is full")
)

func (r Request) validate() error {
	switch {
	case r.Reference == "" || len(r.Reference) > maxFieldLen:
		return &ValidationError{Field: "reference", Message: fmt.Sprintf("is required, at most %d characters", maxFieldLen)}
	case r.Payee == "" || len(r.Payee) > maxFieldLen:
		return &ValidationError{Field: "payee", Message: fmt.Sprintf("is required, at most %d characters", maxFieldLen)}
	case r.AmountCents <= 0 || r.AmountCents > MaxAmountCents:
		return &ValidationError{Field: "amountCents", Message: fmt.Sprintf("must be between 1 and %d", MaxAmountCents)}
	case r.Currency != SupportedCurrency:
		return &ValidationError{Field: "currency", Message: "must be " + SupportedCurrency}
	}
	return nil
}

// Permission is something a caller is allowed to do.
type Permission string

// Permissions granted to calling clients (Keycloak client roles on payments-svc).
const (
	PermWrite Permission = "payments:write"
	PermRead  Permission = "payments:read"
)

// Caller is the authenticated client calling the service.
type Caller struct {
	Subject     string
	ClientID    string
	Permissions []Permission
}

func (c Caller) can(p Permission) bool { return slices.Contains(c.Permissions, p) }

type callerKey struct{}

// WithCaller stores the authenticated caller in the context.
func WithCaller(ctx context.Context, c Caller) context.Context {
	return context.WithValue(ctx, callerKey{}, c)
}

func callerFrom(ctx context.Context) (Caller, bool) {
	c, ok := ctx.Value(callerKey{}).(Caller)
	return c, ok
}

// Store is the persistence port. CreateOnce must be atomic: when key already
// holds a payment it returns that payment and created=false.
type Store interface {
	CreateOnce(ctx context.Context, key string, p Payment) (stored Payment, created bool, err error)
	Get(ctx context.Context, id string) (Payment, error)
}

// Service implements the payment use cases.
type Service struct {
	store Store
	now   func() time.Time
	newID func() string
}

// NewService wires the use cases to their ports.
func NewService(store Store, now func() time.Time, newID func() string) *Service {
	return &Service{store: store, now: now, newID: newID}
}

const maxKeyLen = 255

// Create records a payment exactly once per (client, idempotency key): a
// retry with the same key and request returns the original payment with
// replayed=true; the same key with a different request is ErrKeyReused.
func (s *Service) Create(ctx context.Context, idempotencyKey string, req Request) (p Payment, replayed bool, err error) {
	caller, ok := callerFrom(ctx)
	if !ok || !caller.can(PermWrite) {
		return Payment{}, false, ErrForbidden
	}
	if idempotencyKey == "" || len(idempotencyKey) > maxKeyLen {
		return Payment{}, false, &ValidationError{Field: "Idempotency-Key", Message: fmt.Sprintf("header is required, at most %d characters", maxKeyLen)}
	}
	if err := req.validate(); err != nil {
		return Payment{}, false, err
	}
	candidate := Payment{ID: s.newID(), Request: req, Status: StatusAccepted, Owner: caller.ClientID, CreatedAt: s.now()}
	stored, created, err := s.store.CreateOnce(ctx, caller.ClientID+"\x00"+idempotencyKey, candidate)
	if err != nil {
		return Payment{}, false, err
	}
	if !created && stored.Request != req {
		return Payment{}, false, ErrKeyReused
	}
	return stored, !created, nil
}

// Get returns a payment the calling client created; any other id is ErrNotFound.
func (s *Service) Get(ctx context.Context, id string) (Payment, error) {
	caller, ok := callerFrom(ctx)
	if !ok || (!caller.can(PermRead) && !caller.can(PermWrite)) {
		return Payment{}, ErrForbidden
	}
	p, err := s.store.Get(ctx, id)
	if err != nil {
		return Payment{}, err
	}
	if p.Owner != caller.ClientID {
		return Payment{}, ErrNotFound
	}
	return p, nil
}
