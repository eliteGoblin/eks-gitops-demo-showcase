package main

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/eliteGoblin/eks-gitops-demo/internal/platform"
	"github.com/eliteGoblin/eks-gitops-demo/internal/platform/httpmw"
	kitotel "github.com/eliteGoblin/eks-gitops-demo/internal/platform/otel"
	"github.com/eliteGoblin/eks-gitops-demo/web"
)

// newRouter assembles the shared middleware chain (order documented in httpmw)
// and the routes. Only POST /graphql requires a bearer token: a WebSocket
// (GET /graphql) authenticates in its first message instead.
func newRouter(cfg config, log *slog.Logger, auth httpmw.Authenticator, readiness map[string]platform.Check, gql http.Handler) http.Handler {
	clientIP := rateLimitKey(cfg)
	r := chi.NewRouter()
	r.Use(
		httpmw.RequestID,
		kitotel.HTTPMiddleware(cfg.Telemetry.ServiceName, kitotel.AsPublicEndpoint()), // internet-facing
		httpmw.Recover(log),
		httpmw.AccessLog(log, clientIP),
		httpmw.SecurityHeaders,
		httpmw.CORS(cfg.CORSOrigins),
		httpmw.Limit(maxRequestBytes, requestTimeout),
		httpmw.RateLimit(httpmw.NewRateLimiter(cfg.RateRPS, cfg.RateBurst), clientIP),
	)

	idp := issuerOrigin(cfg.Issuer)
	r.Get("/", withPolicy(pagePolicy(idp), static("index.html")))
	r.Get("/index.css", static("index.css"))
	r.Get("/index.js", static("index.js"))
	r.Get("/oidc.js", static("oidc.js"))
	r.Get("/healthz", platform.Healthz)
	r.Get("/readyz", platform.Readyz(readyTimeout, readiness, log))
	// The pages read the issuer from here, so one image works with any identity provider URL.
	publicCfg, _ := json.Marshal(map[string]string{"issuer": cfg.Issuer, "clientId": "spa"})
	r.Get("/config.json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(publicCfg)
	})
	if cfg.Playground {
		r.Get("/playground", withPolicy(playgroundPolicy(idp), static("playground.html")))
		r.Get("/playground.css", static("playground.css"))
		r.Get("/playground.js", static("playground.js"))
	}
	r.Group(func(r chi.Router) {
		r.Use(httpmw.Authenticate(auth, log))
		r.Method(http.MethodPost, "/graphql", gql)
	})
	r.Method(http.MethodGet, "/graphql", gql) // WebSocket upgrades (graphql-transport-ws)
	return r
}

func rateLimitKey(cfg config) httpmw.KeyFunc {
	switch {
	case cfg.TrustCFConnectingIP:
		return httpmw.CFConnectingIP
	case cfg.TrustProxy:
		return httpmw.ForwardedFor
	default:
		return httpmw.RemoteAddr
	}
}

func static(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, web.FS, name)
	}
}
