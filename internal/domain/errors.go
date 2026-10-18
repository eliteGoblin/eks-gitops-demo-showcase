// Package domain holds the entities, value objects and business rules of the
// support-funding service. It imports nothing from the other layers.
package domain

import (
	"errors"
	"fmt"
)

var (
	// ErrInvalidInput marks input that fails scalar or business validation.
	ErrInvalidInput = errors.New("invalid input")
	// ErrNotFound is returned by repositories when an entity does not exist.
	ErrNotFound = errors.New("not found")
)

// ValidationError is a business validation failure the client is expected to
// act on, so it travels as data rather than as a transport error.
type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string { return fmt.Sprintf("%s: %s", e.Field, e.Message) }

// BudgetExceededError is returned when a claim is larger than what remains.
type BudgetExceededError struct {
	Remaining Money
	Requested Money
}

func (e *BudgetExceededError) Error() string {
	return fmt.Sprintf("budget exceeded: requested %s, remaining %s", e.Requested, e.Remaining)
}

// SelfApprovalNotAllowedError is returned when someone tries to decide a
// request they raised themselves (separation of duties).
type SelfApprovalNotAllowedError struct{}

func (*SelfApprovalNotAllowedError) Error() string {
	return "you cannot decide a request you raised"
}
