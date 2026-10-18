// Package authntest is a minimal OpenID provider for tests: a discovery
// document, a JWKS with one RSA key, and RS256-signed tokens.
package authntest

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// IdP is a test identity provider; URL is its issuer.
type IdP struct {
	URL string
	key *rsa.PrivateKey
}

// NewIdP starts the provider; it stops when the test ends.
func NewIdP(t testing.TB) *IdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := &IdP{key: key}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"issuer": p.URL, "jwks_uri": p.URL + "/jwks"})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	p.URL = srv.URL
	return p
}

// AccessToken returns valid access-token claims for audience, issued to
// client (azp); tests add or override fields before signing.
func (p *IdP) AccessToken(audience, client string) map[string]any {
	return map[string]any{
		"iss": p.URL, "aud": audience, "azp": client, "sub": "sub-" + client, "typ": "Bearer",
		"exp": time.Now().Add(5 * time.Minute).Unix(), "iat": time.Now().Unix(),
	}
}

// Sign returns claims as an RS256 JWT signed with the provider's key.
func (p *IdP) Sign(t testing.TB, claims map[string]any) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: p.key}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "k1"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
