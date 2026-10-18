package oidc

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/eliteGoblin/eks-gitops-demo/internal/app"
	"github.com/eliteGoblin/eks-gitops-demo/internal/platform/authn"
)

const audience = "demo-api"

// idp is a minimal OpenID provider: a discovery document and one RSA key.
type idp struct {
	srv *httptest.Server
	key *rsa.PrivateKey
}

func newIdP(t *testing.T) *idp {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := &idp{key: key}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"issuer": p.srv.URL, "jwks_uri": p.srv.URL + "/jwks"})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		set := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}}
		_ = json.NewEncoder(w).Encode(set)
	})
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	return p
}

func (p *idp) claims() map[string]any {
	return map[string]any{
		"iss": p.srv.URL, "aud": audience, "sub": "user-1", "typ": "Bearer",
		"exp": time.Now().Add(5 * time.Minute).Unix(), "iat": time.Now().Unix(),
		"preferred_username": "zhaolin.sun", "participant_id": "P1",
		"realm_access": map[string]any{"roles": []string{"participant", "offline_access"}},
	}
}

func sign(t *testing.T, alg jose.SignatureAlgorithm, key any, claims map[string]any) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: alg, Key: key}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "k1"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// unsigned builds an "alg: none" token, which must never be accepted.
func unsigned(claims map[string]any) string {
	enc := func(v any) string { b, _ := json.Marshal(v); return base64.RawURLEncoding.EncodeToString(b) }
	return enc(map[string]string{"alg": "none", "typ": "JWT"}) + "." + enc(claims) + "."
}

func discovered(t *testing.T, p *idp, log *slog.Logger, allowedClients ...string) *Verifier {
	t.Helper()
	v := New(authn.Config{Issuer: p.srv.URL, Audience: audience, AllowedClients: allowedClients, Log: log})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := v.Discover(ctx); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestAuthenticateTokens(t *testing.T) {
	p := newIdP(t)
	v := discovered(t, p, slog.New(slog.DiscardHandler))
	otherKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	with := func(k string, val any) map[string]any { c := p.claims(); c[k] = val; return c }
	without := func(k string) map[string]any { c := p.claims(); delete(c, k); return c }

	tests := []struct {
		name  string
		token string
		ok    bool
	}{
		{"valid access token", sign(t, jose.RS256, p.key, p.claims()), true},
		{"wrong issuer", sign(t, jose.RS256, p.key, with("iss", "https://evil.example")), false},
		{"wrong audience", sign(t, jose.RS256, p.key, with("aud", "other-api")), false},
		{"expired", sign(t, jose.RS256, p.key, with("exp", time.Now().Add(-time.Minute).Unix())), false},
		{"alg none", unsigned(p.claims()), false},
		{"HS256 with a shared secret", sign(t, jose.HS256, []byte("0123456789abcdef0123456789abcdef"), p.claims()), false},
		{"signed by another key", sign(t, jose.RS256, otherKey, p.claims()), false},
		{"ID token (typ ID)", sign(t, jose.RS256, p.key, with("typ", "ID")), false},
		{"missing typ", sign(t, jose.RS256, p.key, without("typ")), false},
		{"empty subject", sign(t, jose.RS256, p.key, with("sub", "")), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, err := v.Authenticate(context.Background(), tc.token)
			if tc.ok != (err == nil) {
				t.Fatalf("ok=%v, got err %v", tc.ok, err)
			}
			if !tc.ok {
				return
			}
			pr, _ := app.PrincipalFrom(ctx)
			if pr.Subject != "user-1" || pr.ParticipantID != "P1" || !pr.HasRole(app.RoleParticipant) {
				t.Fatalf("principal = %+v", pr)
			}
		})
	}
}

func TestTokenWithoutKnownRolesIsLogged(t *testing.T) {
	p := newIdP(t)
	var buf bytes.Buffer
	v := discovered(t, p, slog.New(slog.NewTextHandler(&buf, nil)))
	c := p.claims()
	c["realm_access"] = map[string]any{"roles": []string{"PARTICIPANT"}} // wrong case: maps to nothing
	if _, err := v.Authenticate(context.Background(), sign(t, jose.RS256, p.key, c)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "no known roles") {
		t.Fatalf("a token that maps to zero roles must be logged, log was %q", buf.String())
	}
}

func TestNotReadyBeforeDiscovery(t *testing.T) {
	v := New(authn.Config{Issuer: "https://id.example/realms/demo", Audience: audience})
	if _, err := v.Authenticate(context.Background(), "x.y.z"); !errors.Is(err, ErrNotReady) {
		t.Fatalf("Authenticate before discovery: %v", err)
	}
	if err := v.Ready(context.Background()); !errors.Is(err, ErrNotReady) {
		t.Fatalf("Ready before discovery: %v", err)
	}
}

func TestDiscoverStopsWithContext(t *testing.T) {
	down := httptest.NewServer(http.NotFoundHandler())
	down.Close()
	v := New(authn.Config{Issuer: down.URL, Audience: audience})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := v.Discover(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Discover must give up when the context ends, got %v", err)
	}
}

func TestAllowedClients(t *testing.T) {
	p := newIdP(t)
	v := discovered(t, p, slog.New(slog.DiscardHandler), "spa")
	c := p.claims()
	c["azp"] = "spa"
	if _, err := v.Authenticate(context.Background(), sign(t, jose.RS256, p.key, c)); err != nil {
		t.Fatalf("token from an allowed client: %v", err)
	}
	c["azp"] = "some-other-client"
	if _, err := v.Authenticate(context.Background(), sign(t, jose.RS256, p.key, c)); !errors.Is(err, authn.ErrClientNotAllowed) {
		t.Fatalf("token from a client not on the allowlist must be rejected, got %v", err)
	}
}
