// Package app holds the use cases. It depends only on the domain and declares
// the ports (small interfaces) it needs; adapters implement them.
package app

import (
	"context"
	"errors"
	"time"
)

var (
	// ErrForbidden is returned when the policy denies an action. Callers never
	// learn why, and "not found" and "not permitted" look the same.
	ErrForbidden = errors.New("not permitted")
	// ErrUnauthenticated is returned when no verified principal is present.
	ErrUnauthenticated = errors.New("login required")
)

// Role is a realm role as it appears in the access token (lowercase).
type Role string

// The realm roles the app understands; any other token role is ignored.
const (
	RoleParticipant Role = "participant"
	RoleNominee     Role = "nominee"
	RoleProvider    Role = "provider"
	RoleAgencyStaff Role = "agency_staff"
)

// KnownRoles lists the roles the app understands, in display order.
var KnownRoles = []Role{RoleParticipant, RoleNominee, RoleProvider, RoleAgencyStaff}

// Principal is the verified identity of the caller.
type Principal struct {
	Subject       string
	Username      string
	Roles         map[Role]bool
	ParticipantID string // set when the user is linked to a participant
	NomineeID     string
	ProviderID    string
	ExpiresAt     time.Time // when the credential expires; zero when unknown
}

// HasRole reports whether the principal holds the role.
func (p Principal) HasRole(r Role) bool { return p.Roles[r] }

// RoleNames returns the held roles in a stable order.
func (p Principal) RoleNames() []Role {
	out := []Role{}
	for _, r := range KnownRoles {
		if p.Roles[r] {
			out = append(out, r)
		}
	}
	return out
}

type principalKey struct{}

// WithPrincipal stores the principal in the context.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFrom returns the principal stored by the authentication middleware.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}
