package main

import (
	"net/http"
	"net/url"
	"strings"
)

// graphiqlOrigin serves the pinned GraphiQL, React and stylesheet files the
// playground loads (each with a Subresource Integrity hash in the page).
const graphiqlOrigin = "https://unpkg.com"

// pagePolicy is the Content-Security-Policy of the token walkthrough page:
// its scripts and styles come only from this server, and the only other
// origin it talks to is the identity provider (discovery and token calls).
// The pages have no forms (logins navigate to the identity provider rather
// than post), so form-action is 'none'.
func pagePolicy(idp string) string {
	return policy(
		"default-src 'self'",
		"script-src 'self'",
		"style-src 'self'",
		"img-src 'self' data:",
		"connect-src 'self' "+idp,
		"form-action 'none'",
		"frame-ancestors 'none'",
		"base-uri 'none'",
		"object-src 'none'",
	)
}

// playgroundPolicy is pagePolicy plus GraphiQL's files from graphiqlOrigin.
// Neither scripts nor styles need 'unsafe-inline' or 'unsafe-eval': GraphiQL
// styles elements through the CSSOM, which a policy does not restrict
// (checked in Chrome with the docs explorer, autocomplete and prettify).
func playgroundPolicy(idp string) string {
	return policy(
		"default-src 'self'",
		"script-src 'self' "+graphiqlOrigin,
		"style-src 'self' "+graphiqlOrigin,
		"font-src 'self' "+graphiqlOrigin+" data:",
		"img-src 'self' data:",
		"connect-src 'self' "+idp,
		"form-action 'none'",
		"frame-ancestors 'none'",
		"base-uri 'none'",
		"object-src 'none'",
	)
}

func policy(directives ...string) string { return strings.Join(directives, "; ") }

// withPolicy replaces the default Content-Security-Policy for one page.
func withPolicy(csp string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", csp)
		next(w, r)
	}
}

// issuerOrigin is the scheme://host[:port] of the OIDC issuer URL.
func issuerOrigin(issuer string) string {
	u, err := url.Parse(issuer)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "'none'" // config validation rejects such an issuer; never widen the policy
	}
	return u.Scheme + "://" + u.Host
}
