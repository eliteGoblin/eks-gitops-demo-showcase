// Package authn is the service kit's inbound authentication: it verifies
// OpenID Connect access tokens (RS256 signature via JWKS, issuer, audience,
// expiry, typ, and optionally the calling client) and hands the verified
// token to a per-service mapper that builds that service's own identity.
//
// Every service in the repo uses it the same way: New → Discover in the
// background → Authenticator(mapper) mounted with httpmw.Authenticate.
package authn

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"sync/atomic"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
)

var (
	// ErrNotReady is returned until the issuer's discovery document has been read.
	ErrNotReady = errors.New("identity provider not discovered yet")
	// ErrNotAccessToken is returned for ID tokens and other non-access tokens.
	ErrNotAccessToken = errors.New("not an access token")
	// ErrNoSubject is returned for a token without a subject.
	ErrNoSubject = errors.New("token has no subject")
	// ErrClientNotAllowed is returned when the token was issued to a client
	// (its azp claim) that is not on the service's allowlist.
	ErrClientNotAllowed = errors.New("token issued to a client that is not allowed")
)

const (
	discoveryBackoff = 2 * time.Second
	httpTimeout      = 5 * time.Second
)

// Config describes which tokens a service accepts.
type Config struct {
	Issuer   string
	Audience string
	// AllowedClients lists the client ids (azp) whose tokens are accepted.
	// Empty accepts any client that obtained a token for Audience.
	AllowedClients []string
	Log            *slog.Logger
	// Transport carries discovery and signing-key requests (e.g. an
	// instrumented one); nil uses http.DefaultTransport.
	Transport http.RoundTripper
}

// Verifier verifies access tokens. It is usable before the identity provider
// answers: Discover runs in the background while the server already listens,
// and until it succeeds Verify and Ready report ErrNotReady.
type Verifier struct {
	cfg    Config
	client *http.Client
	state  atomic.Pointer[discovery]
	jwksOK atomic.Bool
}

type discovery struct {
	verifier *gooidc.IDTokenVerifier
	jwksURL  string
}

// New returns a verifier for cfg. Call Discover before it can verify anything.
func New(cfg Config) *Verifier {
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	return &Verifier{cfg: cfg, client: &http.Client{Timeout: httpTimeout, Transport: cfg.Transport}}
}

// Discover reads the issuer's discovery document, retrying while the identity
// provider starts, until it succeeds or ctx ends. It then warms the signing
// keys once so /readyz can report them.
func (v *Verifier) Discover(ctx context.Context) error {
	for attempt := 1; ; attempt++ {
		err := v.discoverOnce(ctx)
		if err == nil {
			if err := v.Ready(ctx); err != nil {
				v.cfg.Log.WarnContext(ctx, "signing keys not fetched yet; /readyz retries", "err", err)
			}
			return nil
		}
		v.cfg.Log.WarnContext(ctx, "oidc discovery failed, retrying", "issuer", v.cfg.Issuer, "attempt", attempt, "err", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(discoveryBackoff):
		}
	}
}

func (v *Verifier) discoverOnce(ctx context.Context) error {
	// go-oidc keeps this context for background JWKS refreshes: give it the
	// timeout-bound client and detach it from the caller's cancellation.
	providerCtx := gooidc.ClientContext(context.WithoutCancel(ctx), v.client)
	provider, err := gooidc.NewProvider(providerCtx, v.cfg.Issuer)
	if err != nil {
		return err
	}
	var meta struct {
		JWKSURL string `json:"jwks_uri"`
	}
	if err := provider.Claims(&meta); err != nil {
		return fmt.Errorf("read discovery document: %w", err)
	}
	v.state.Store(&discovery{
		verifier: provider.Verifier(&gooidc.Config{ClientID: v.cfg.Audience, SupportedSigningAlgs: []string{gooidc.RS256}}),
		jwksURL:  meta.JWKSURL,
	})
	return nil
}

// Ready reports whether discovery succeeded and the signing keys have been
// fetched at least once. Afterwards it no longer depends on the provider.
func (v *Verifier) Ready(ctx context.Context) error {
	if v.jwksOK.Load() {
		return nil
	}
	st := v.state.Load()
	if st == nil {
		return ErrNotReady
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, st.jwksURL, nil)
	if err != nil {
		return err
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return fmt.Errorf("fetch jwks: %w", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch jwks: HTTP %d", resp.StatusCode)
	}
	v.jwksOK.Store(true)
	return nil
}

// Token is a verified access token.
type Token struct {
	Subject  string
	ClientID string // the azp claim: the client the token was issued to
	tok      *gooidc.IDToken
}

// Claims decodes the token's claims into dst, for service-specific fields.
func (t Token) Claims(dst any) error { return t.tok.Claims(dst) }

// Verify checks the RS256 signature, issuer, audience and expiry, then that
// the token is an access token (typ "Bearer": ID tokens are meant for the
// client, not APIs) with a subject, issued to an allowed client.
func (v *Verifier) Verify(ctx context.Context, raw string) (Token, error) {
	st := v.state.Load()
	if st == nil {
		return Token{}, ErrNotReady
	}
	tok, err := st.verifier.Verify(ctx, raw)
	if err != nil {
		return Token{}, err
	}
	var std struct {
		Typ string `json:"typ"`
		Azp string `json:"azp"`
	}
	if err := tok.Claims(&std); err != nil {
		return Token{}, fmt.Errorf("decode claims: %w", err)
	}
	switch {
	case std.Typ != "Bearer":
		return Token{}, ErrNotAccessToken
	case tok.Subject == "":
		return Token{}, ErrNoSubject
	case len(v.cfg.AllowedClients) > 0 && !slices.Contains(v.cfg.AllowedClients, std.Azp):
		return Token{}, fmt.Errorf("%w: %q", ErrClientNotAllowed, std.Azp)
	}
	return Token{Subject: tok.Subject, ClientID: std.Azp, tok: tok}, nil
}

// Mapper turns a verified token into the service's own identity, stored in
// the returned context (e.g. demo-api's principal, payments-svc's caller).
type Mapper func(ctx context.Context, tok Token) (context.Context, error)

// Authenticator verifies a bearer token and maps it; it satisfies
// httpmw.Authenticator.
type Authenticator struct {
	v      *Verifier
	mapper Mapper
}

// Authenticator returns an Authenticator that maps verified tokens with m.
func (v *Verifier) Authenticator(m Mapper) Authenticator {
	return Authenticator{v: v, mapper: m}
}

// Authenticate verifies raw and returns the mapper's context.
func (a Authenticator) Authenticate(ctx context.Context, raw string) (context.Context, error) {
	tok, err := a.v.Verify(ctx, raw)
	if err != nil {
		return nil, err
	}
	return a.mapper(ctx, tok)
}
