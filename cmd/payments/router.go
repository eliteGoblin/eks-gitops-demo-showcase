package main

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/eliteGoblin/eks-gitops-demo/internal/payments/httpapi"
	"github.com/eliteGoblin/eks-gitops-demo/internal/platform"
	"github.com/eliteGoblin/eks-gitops-demo/internal/platform/httpmw"
	kitotel "github.com/eliteGoblin/eks-gitops-demo/internal/platform/otel"
)

// newRouter assembles the kit's middleware chain (same order as demo-api,
// with REST error envelopes) and the routes. Every /payments route requires
// a service token.
func newRouter(cfg config, log *slog.Logger, auth httpmw.Authenticator, readiness map[string]platform.Check, svc httpapi.Service) http.Handler {
	key := httpmw.RemoteAddr
	if cfg.TrustProxy {
		key = httpmw.ForwardedFor
	}
	r := chi.NewRouter()
	r.Use(
		httpmw.ErrorFormat(platform.RESTErrors),
		httpmw.RequestID,
		kitotel.HTTPMiddleware(cfg.Telemetry.ServiceName),
		httpmw.Recover(log),
		httpmw.AccessLog(log, key),
		httpmw.SecurityHeaders,
		httpmw.Limit(maxRequestBytes, requestTimeout),
		httpmw.RateLimit(httpmw.NewRateLimiter(cfg.RateRPS, cfg.RateBurst), key),
	)
	// Unknown routes and methods answer in the same envelope as everything else.
	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		platform.WriteError(w, r, http.StatusNotFound, platform.CodeNotFound, "no such route")
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		platform.WriteError(w, r, http.StatusMethodNotAllowed, platform.CodeBadUserInput, "method not allowed")
	})
	r.Get("/healthz", platform.Healthz)
	r.Get("/readyz", platform.Readyz(readyTimeout, readiness, log))
	r.Group(func(r chi.Router) {
		r.Use(httpmw.Authenticate(auth, log))
		httpapi.Mount(r, svc, log)
	})
	return r
}
