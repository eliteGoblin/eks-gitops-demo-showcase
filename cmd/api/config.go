package main

import (
	"strings"

	"github.com/eliteGoblin/eks-gitops-demo/internal/adapter/graphql"
	"github.com/eliteGoblin/eks-gitops-demo/internal/platform"
	kitotel "github.com/eliteGoblin/eks-gitops-demo/internal/platform/otel"
)

// config is the API's runtime configuration. Defaults are safe for
// production: forgetting a variable never turns on a development feature.
type config struct {
	Issuer        string
	Audience      string
	Port          string
	Env           string // "prod" (default) or "dev"; dev only allows an http issuer for local Keycloak
	Playground    bool   // serve /playground (GraphiQL); explicit opt-in
	Introspection bool   // allow __schema/__type; explicit opt-in
	CORSOrigins   []string
	TrustProxy    bool // true behind exactly one proxy that sets X-Forwarded-For (the ALB)
	RateRPS       float64
	RateBurst     int
	Store         string // storeMemory (seeded, per pod) or storePostgres
	DatabaseURL   string // libpq URL or key/value; empty means the PG* environment variables
	DBMaxConns    int    // pool size per pod; HPA max x this must fit the database's connection limit

	// TrustCFConnectingIP keys rate limiting on CF-Connecting-IP, the client
	// address Cloudflare's edge sets (cloudflared passes it through). Enable it
	// only after the ALB is retired and the NetworkPolicy makes cloudflared the
	// only peer: until then anyone can reach the API through the ALB with a
	// forged header. Never together with TrustProxy.
	TrustCFConnectingIP bool

	PersistedQueries    string // persistedOff, persistedAPQ (default) or persistedAllowlist
	PersistedQueryCache int    // APQ documents remembered per pod
	MaxDocumentBytes    int    // largest GraphQL document accepted (bounds the document caches)

	// AllowedClients lists the OIDC clients (azp) whose tokens are accepted; empty accepts any.
	AllowedClients []string

	Telemetry kitotel.Config // OTEL_*; exporting is off unless OTEL_EXPORTER_OTLP_ENDPOINT is set
	Payments  paymentsConfig // PAYMENTS_*; approved claims are paid only when enabled
	Partner   partnerConfig  // PARTNER_*; the demo partner API key (secret delivery and rotation)
}

// paymentsConfig is how demo-api reaches payments-svc: with its own
// client-credentials token (service account demo-api-svc), never the user's.
type paymentsConfig struct {
	Enabled      bool
	URL          string // payments-svc base URL
	TokenURL     string // the issuer's token endpoint unless set
	ClientID     string
	ClientSecret string
}

const (
	storeMemory   = "memory"
	storePostgres = "postgres"
)

func loadConfig(env *platform.Env) (config, error) {
	cfg := config{
		Issuer:        env.URL("OIDC_ISSUER", "http://localhost:8080/realms/demo"),
		Audience:      env.String("OIDC_AUDIENCE", "demo-api"),
		Port:          env.String("PORT", "8082"),
		Env:           env.OneOf("ENV", "prod", "dev", "prod"),
		Playground:    env.Bool("PLAYGROUND_ENABLED", false),
		Introspection: env.Bool("INTROSPECTION_ENABLED", false),
		CORSOrigins:   env.List("CORS_ALLOWED_ORIGINS"),
		TrustProxy:    env.Bool("TRUST_PROXY", false),
		RateRPS:       env.Float("RATE_LIMIT_RPS", 20),
		RateBurst:     env.Int("RATE_LIMIT_BURST", 40),
		Store:         env.OneOf("STORE", storeMemory, storeMemory, storePostgres),
		DatabaseURL:   env.String("DATABASE_URL", ""),
		DBMaxConns:    env.Int("DB_MAX_CONNS", 8),

		TrustCFConnectingIP: env.Bool("TRUST_CF_CONNECTING_IP", false),

		PersistedQueries:    env.OneOf("PERSISTED_QUERIES", persistedAPQ, persistedOff, persistedAPQ, persistedAllowlist),
		PersistedQueryCache: env.Int("PERSISTED_QUERIES_CACHE", 1000),
		MaxDocumentBytes:    env.Int("GRAPHQL_MAX_DOCUMENT_BYTES", graphql.DefaultMaxDocumentBytes),

		AllowedClients: env.List("OIDC_ALLOWED_CLIENTS"),
		Telemetry:      kitotel.FromEnv(env, "demo-api"),
		Partner:        partnerConfig{APIKey: env.String("PARTNER_API_KEY", ""), KeyFile: env.String("PARTNER_KEY_FILE", "")},
	}
	cfg.Payments = loadPayments(env, cfg.Issuer)
	if cfg.DBMaxConns < 1 {
		env.Errorf("DB_MAX_CONNS must be at least 1, got %d", cfg.DBMaxConns)
	}
	if cfg.TrustProxy && cfg.TrustCFConnectingIP {
		env.Errorf("TRUST_PROXY and TRUST_CF_CONNECTING_IP are mutually exclusive: trust the header of the one proxy in front of the API (X-Forwarded-For from the ALB, or CF-Connecting-IP from cloudflared)")
	}
	if cfg.PersistedQueryCache < 1 {
		env.Errorf("PERSISTED_QUERIES_CACHE must be at least 1, got %d", cfg.PersistedQueryCache)
	}
	if cfg.MaxDocumentBytes < minDocumentBytes || cfg.MaxDocumentBytes > maxRequestBytes {
		env.Errorf("GRAPHQL_MAX_DOCUMENT_BYTES must be between %d and the request body limit %d, got %d", minDocumentBytes, maxRequestBytes, cfg.MaxDocumentBytes)
	}
	if cfg.Env == "prod" && !strings.HasPrefix(cfg.Issuer, "https://") {
		env.Errorf("OIDC_ISSUER must use https when ENV=prod (set ENV=dev for a local http Keycloak), got %q", cfg.Issuer)
	}
	return cfg, env.Err()
}

func loadPayments(env *platform.Env, issuer string) paymentsConfig {
	p := paymentsConfig{Enabled: env.Bool("PAYMENTS_ENABLED", false)}
	if !p.Enabled {
		return p
	}
	p.URL = env.URL("PAYMENTS_URL", "http://localhost:8083")
	p.TokenURL = env.URL("PAYMENTS_TOKEN_URL", strings.TrimSuffix(issuer, "/")+"/protocol/openid-connect/token")
	p.ClientID = env.String("PAYMENTS_CLIENT_ID", "demo-api-svc")
	p.ClientSecret = env.Required("PAYMENTS_CLIENT_SECRET")
	return p
}
