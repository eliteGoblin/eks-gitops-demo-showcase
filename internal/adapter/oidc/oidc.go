// Package oidc maps verified OpenID Connect access tokens to demo-api's
// app.Principal. Token verification itself (signature, issuer, audience,
// expiry, typ, allowed clients) is the shared kit's platform/authn.
package oidc

import (
	"context"
	"log/slog"
	"time"

	"github.com/eliteGoblin/eks-gitops-demo/internal/app"
	"github.com/eliteGoblin/eks-gitops-demo/internal/platform/authn"
)

// Errors re-exported from the kit so callers can match them without importing it.
var (
	ErrNotReady       = authn.ErrNotReady
	ErrNotAccessToken = authn.ErrNotAccessToken
	ErrNoSubject      = authn.ErrNoSubject
)

// Verifier implements httpmw.Authenticator for demo-api. Discover and Ready
// come from the embedded kit verifier.
type Verifier struct {
	*authn.Verifier
	auth authn.Authenticator
	log  *slog.Logger
}

// New returns demo-api's verifier. Call Discover (typically in a goroutine)
// before it can verify anything.
func New(cfg authn.Config) *Verifier {
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	v := &Verifier{Verifier: authn.New(cfg), log: cfg.Log}
	v.auth = v.Authenticator(v.principal)
	return v
}

// Authenticate verifies the token and stores the principal in the context.
func (v *Verifier) Authenticate(ctx context.Context, raw string) (context.Context, error) {
	return v.auth.Authenticate(ctx, raw)
}

func (v *Verifier) principal(ctx context.Context, tok authn.Token) (context.Context, error) {
	var c Claims
	if err := tok.Claims(&c); err != nil {
		return nil, err
	}
	p := c.Principal()
	if len(p.Roles) == 0 {
		// Usually a realm/role-name mismatch: every request will be FORBIDDEN.
		v.log.WarnContext(ctx, "verified token maps to no known roles", "sub", p.Subject, "token_roles", c.RealmAccess.Roles)
	}
	return app.WithPrincipal(ctx, p), nil
}

// Claims is the subset of the Keycloak access token demo-api uses.
type Claims struct {
	Subject           string `json:"sub"`
	PreferredUsername string `json:"preferred_username"`
	ParticipantID     string `json:"participant_id"`
	NomineeID         string `json:"nominee_id"`
	ProviderID        string `json:"provider_id"`
	Expiry            int64  `json:"exp"` // seconds since the epoch
	RealmAccess       struct {
		Roles []string `json:"roles"`
	} `json:"realm_access"`
}

// Principal maps the claims, keeping only the realm roles the app knows.
func (c Claims) Principal() app.Principal {
	known := map[app.Role]bool{}
	for _, r := range app.KnownRoles {
		known[r] = true
	}
	roles := map[app.Role]bool{}
	for _, r := range c.RealmAccess.Roles {
		if known[app.Role(r)] {
			roles[app.Role(r)] = true
		}
	}
	p := app.Principal{
		Subject: c.Subject, Username: c.PreferredUsername, Roles: roles,
		ParticipantID: c.ParticipantID, NomineeID: c.NomineeID, ProviderID: c.ProviderID,
	}
	if c.Expiry > 0 {
		p.ExpiresAt = time.Unix(c.Expiry, 0)
	}
	return p
}
