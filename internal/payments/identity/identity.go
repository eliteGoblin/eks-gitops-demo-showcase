// Package identity maps verified Keycloak access tokens to payments.Caller.
package identity

import (
	"context"
	"errors"

	"github.com/eliteGoblin/eks-gitops-demo/internal/payments"
	"github.com/eliteGoblin/eks-gitops-demo/internal/platform/authn"
)

var known = []payments.Permission{payments.PermWrite, payments.PermRead}

// ErrNoClient rejects a token without an azp claim: the client id scopes
// idempotency keys and payment ownership, so it can never be empty.
var ErrNoClient = errors.New("token names no client (azp)")

// Mapper reads the caller's permissions from the client roles the token
// carries for resourceClient (Keycloak's resource_access.<client>.roles),
// keeping only permissions the service knows.
func Mapper(resourceClient string) authn.Mapper {
	return func(ctx context.Context, tok authn.Token) (context.Context, error) {
		c, err := caller(tok.Subject, tok.ClientID, tok, resourceClient)
		if err != nil {
			return nil, err
		}
		return payments.WithCaller(ctx, c), nil
	}
}

type claimsDecoder interface{ Claims(dst any) error }

func caller(subject, clientID string, claims claimsDecoder, resourceClient string) (payments.Caller, error) {
	if clientID == "" {
		return payments.Caller{}, ErrNoClient
	}
	var c struct {
		ResourceAccess map[string]struct {
			Roles []string `json:"roles"`
		} `json:"resource_access"`
	}
	if err := claims.Claims(&c); err != nil {
		return payments.Caller{}, err
	}
	var perms []payments.Permission
	for _, role := range c.ResourceAccess[resourceClient].Roles {
		for _, p := range known {
			if role == string(p) {
				perms = append(perms, p)
			}
		}
	}
	return payments.Caller{Subject: subject, ClientID: clientID, Permissions: perms}, nil
}
