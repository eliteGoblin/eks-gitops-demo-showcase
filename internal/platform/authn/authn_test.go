package authn

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

const audience = "payments-svc"

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
		"iss": p.srv.URL, "aud": audience, "sub": "service-account-1", "typ": "Bearer", "azp": "demo-api-svc",
		"exp": time.Now().Add(5 * time.Minute).Unix(), "iat": time.Now().Unix(),
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

func unsigned(claims map[string]any) string {
	enc := func(v any) string { b, _ := json.Marshal(v); return base64.RawURLEncoding.EncodeToString(b) }
	return enc(map[string]string{"alg": "none", "typ": "JWT"}) + "." + enc(claims) + "."
}

func discovered(t *testing.T, cfg Config) *Verifier {
	t.Helper()
	v := New(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := v.Discover(ctx); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestVerify(t *testing.T) {
	p := newIdP(t)
	otherKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	with := func(k string, val any) map[string]any { c := p.claims(); c[k] = val; return c }
	without := func(k string) map[string]any { c := p.claims(); delete(c, k); return c }

	tests := []struct {
		name    string
		allowed []string
		token   string
		wantErr error // nil means accepted; errAny means any error
	}{
		{"valid, no client allowlist", nil, sign(t, jose.RS256, p.key, p.claims()), nil},
		{"valid, client on the allowlist", []string{"demo-api-svc"}, sign(t, jose.RS256, p.key, p.claims()), nil},
		{"client not on the allowlist", []string{"demo-api-svc"}, sign(t, jose.RS256, p.key, with("azp", "spa")), ErrClientNotAllowed},
		{"no azp with an allowlist", []string{"demo-api-svc"}, sign(t, jose.RS256, p.key, without("azp")), ErrClientNotAllowed},
		{"wrong issuer", nil, sign(t, jose.RS256, p.key, with("iss", "https://evil.example")), errAny},
		{"wrong audience", nil, sign(t, jose.RS256, p.key, with("aud", "demo-api")), errAny},
		{"expired", nil, sign(t, jose.RS256, p.key, with("exp", time.Now().Add(-time.Minute).Unix())), errAny},
		{"alg none", nil, unsigned(p.claims()), errAny},
		{"HS256 with a shared secret", nil, sign(t, jose.HS256, []byte("0123456789abcdef0123456789abcdef"), p.claims()), errAny},
		{"signed by another key", nil, sign(t, jose.RS256, otherKey, p.claims()), errAny},
		{"ID token (typ ID)", nil, sign(t, jose.RS256, p.key, with("typ", "ID")), ErrNotAccessToken},
		{"missing typ", nil, sign(t, jose.RS256, p.key, without("typ")), ErrNotAccessToken},
		{"empty subject", nil, sign(t, jose.RS256, p.key, with("sub", "")), ErrNoSubject},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := discovered(t, Config{Issuer: p.srv.URL, Audience: audience, AllowedClients: tc.allowed, Log: slog.New(slog.DiscardHandler)})
			tok, err := v.Verify(context.Background(), tc.token)
			switch {
			case tc.wantErr == nil && err != nil:
				t.Fatalf("want accepted, got %v", err)
			case tc.wantErr != nil && err == nil:
				t.Fatal("want rejected, got accepted")
			case tc.wantErr != nil && !errors.Is(tc.wantErr, errAny) && !errors.Is(err, tc.wantErr):
				t.Fatalf("want %v, got %v", tc.wantErr, err)
			}
			if tc.wantErr == nil && (tok.Subject != "service-account-1" || tok.ClientID != "demo-api-svc") {
				t.Fatalf("token = %+v", tok)
			}
		})
	}
}

var errAny = errors.New("any error")

func TestTokenClaimsDecodesServiceSpecificFields(t *testing.T) {
	p := newIdP(t)
	v := discovered(t, Config{Issuer: p.srv.URL, Audience: audience, Log: slog.New(slog.DiscardHandler)})
	c := p.claims()
	c["resource_access"] = map[string]any{"payments-svc": map[string]any{"roles": []string{"payments:write"}}}
	tok, err := v.Verify(context.Background(), sign(t, jose.RS256, p.key, c))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		ResourceAccess map[string]struct {
			Roles []string `json:"roles"`
		} `json:"resource_access"`
	}
	if err := tok.Claims(&got); err != nil || len(got.ResourceAccess["payments-svc"].Roles) != 1 {
		t.Fatalf("claims = %+v, %v", got, err)
	}
}

func TestAuthenticatorMapsVerifiedTokens(t *testing.T) {
	p := newIdP(t)
	v := discovered(t, Config{Issuer: p.srv.URL, Audience: audience, Log: slog.New(slog.DiscardHandler)})
	type key struct{}
	auth := v.Authenticator(func(ctx context.Context, tok Token) (context.Context, error) {
		return context.WithValue(ctx, key{}, tok.Subject), nil
	})
	ctx, err := auth.Authenticate(context.Background(), sign(t, jose.RS256, p.key, p.claims()))
	if err != nil || ctx.Value(key{}) != "service-account-1" {
		t.Fatalf("mapped ctx value = %v, err %v", ctx.Value(key{}), err)
	}
	if _, err := auth.Authenticate(context.Background(), "not-a-token"); err == nil {
		t.Fatal("an invalid token must not reach the mapper")
	}
	mapErr := errors.New("mapper refused")
	refuse := v.Authenticator(func(context.Context, Token) (context.Context, error) { return nil, mapErr })
	if _, err := refuse.Authenticate(context.Background(), sign(t, jose.RS256, p.key, p.claims())); !errors.Is(err, mapErr) {
		t.Fatalf("mapper errors must propagate, got %v", err)
	}
}

func TestNotReadyBeforeDiscovery(t *testing.T) {
	v := New(Config{Issuer: "https://id.example/realms/demo", Audience: audience, Log: slog.New(slog.DiscardHandler)})
	if _, err := v.Verify(context.Background(), "x.y.z"); !errors.Is(err, ErrNotReady) {
		t.Fatalf("Verify before discovery: %v", err)
	}
	if err := v.Ready(context.Background()); !errors.Is(err, ErrNotReady) {
		t.Fatalf("Ready before discovery: %v", err)
	}
}

func TestDiscoverStopsWithContext(t *testing.T) {
	down := httptest.NewServer(http.NotFoundHandler())
	down.Close()
	v := New(Config{Issuer: down.URL, Audience: audience, Log: slog.New(slog.DiscardHandler)})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := v.Discover(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Discover must give up when the context ends, got %v", err)
	}
}

func TestReadyIsStickyOnceKeysWereFetched(t *testing.T) {
	status := http.StatusInternalServerError
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }))
	defer srv.Close()
	v := New(Config{Issuer: srv.URL, Audience: audience, Log: slog.New(slog.DiscardHandler)})
	v.client = srv.Client()
	v.state.Store(&discovery{jwksURL: srv.URL})
	if err := v.Ready(context.Background()); err == nil {
		t.Fatal("500 from JWKS must not be ready")
	}
	status = http.StatusOK
	if err := v.Ready(context.Background()); err != nil {
		t.Fatalf("ready: %v", err)
	}
	srv.Close()
	if err := v.Ready(context.Background()); err != nil {
		t.Fatal("once fetched, readiness must not depend on the IdP staying up")
	}
}
