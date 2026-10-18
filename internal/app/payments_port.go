package app

import (
	"context"
	"errors"
)

// PaymentGateway asks the payment processor (payments-svc) to pay an approved
// claim. Implemented by adapter/paymentsclient; a use case wires it in once
// the claim/support-request flow is ready.
type PaymentGateway interface {
	RequestPayment(ctx context.Context, req PaymentRequest) (PaymentReceipt, error)
}

// PaymentRequest is one payment. IdempotencyKey makes retries safe: the
// processor pays a key at most once (use the claim id).
type PaymentRequest struct {
	IdempotencyKey string
	Reference      string
	Payee          string
	AmountCents    int64
	Currency       string
}

// PaymentReceipt is the processor's answer.
type PaymentReceipt struct {
	PaymentID string
	Status    string
	Replayed  bool // the processor had already recorded this key
}

var (
	// ErrPaymentRejected means the processor refused the request; retrying won't help.
	ErrPaymentRejected = errors.New("payment rejected")
	// ErrPaymentUnavailable means the processor could not be reached (or the
	// circuit breaker is open); the caller may retry later with the same key.
	ErrPaymentUnavailable = errors.New("payment processor unavailable")
	// ErrPaymentNotRecorded: the processor answered, but its result could not
	// be stored on the claim. Money may have moved without a record of it, so
	// this is a server failure for operators, never a client error.
	ErrPaymentNotRecorded = errors.New("payment result not recorded")
)
