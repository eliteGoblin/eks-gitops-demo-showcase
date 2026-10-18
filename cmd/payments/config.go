package main

import (
	"strings"

	"github.com/eliteGoblin/eks-gitops-demo/internal/platform"
	kitotel "github.com/eliteGoblin/eks-gitops-demo/internal/platform/otel"
)

// config is payments-svc's runtime configuration. Defaults are production-safe.
type config struct {
	Issuer         string
	Audience       string
	AllowedClients []string // only these OIDC clients (azp) may call the service
	Port           string
	Env            string // "prod" (default) or "dev"; dev only allows an http issuer
	TrustProxy     bool
	RateRPS        float64
	RateBurst      int
	Telemetry      kitotel.Config // OTEL_*; exporting is off unless OTEL_EXPORTER_OTLP_ENDPOINT is set
}

func loadConfig(env *platform.Env) (config, error) {
	cfg := config{
		Issuer:         env.Required("OIDC_ISSUER"),
		Audience:       env.String("OIDC_AUDIENCE", "payments-svc"),
		AllowedClients: env.List("OIDC_ALLOWED_CLIENTS"),
		Port:           env.String("PORT", "8083"),
		Env:            env.OneOf("ENV", "prod", "dev", "prod"),
		TrustProxy:     env.Bool("TRUST_PROXY", false),
		RateRPS:        env.Float("RATE_LIMIT_RPS", 50),
		RateBurst:      env.Int("RATE_LIMIT_BURST", 100),
		Telemetry:      kitotel.FromEnv(env, "payments-svc"),
	}
	if len(cfg.AllowedClients) == 0 {
		cfg.AllowedClients = []string{"demo-api-svc"}
	}
	if cfg.Issuer != "" { // Required already reported a missing issuer
		env.URL("OIDC_ISSUER", "")
	}
	if cfg.Env == "prod" && cfg.Issuer != "" && !strings.HasPrefix(cfg.Issuer, "https://") {
		env.Errorf("OIDC_ISSUER must use https when ENV=prod (set ENV=dev for a local http Keycloak), got %q", cfg.Issuer)
	}
	return cfg, env.Err()
}
