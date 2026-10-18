// Command api serves the GraphQL API, the demo SPA and (opt-in) the
// playground. `api migrate` applies the database migrations and exits.
// This file is the composition root: config → adapters → use cases → server.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/eliteGoblin/eks-gitops-demo/internal/adapter/audit"
	"github.com/eliteGoblin/eks-gitops-demo/internal/adapter/graphql"
	"github.com/eliteGoblin/eks-gitops-demo/internal/adapter/oidc"
	"github.com/eliteGoblin/eks-gitops-demo/internal/app"
	"github.com/eliteGoblin/eks-gitops-demo/internal/platform"
	"github.com/eliteGoblin/eks-gitops-demo/internal/platform/authn"
	kitotel "github.com/eliteGoblin/eks-gitops-demo/internal/platform/otel"
	"github.com/eliteGoblin/eks-gitops-demo/internal/platform/otel/otelgql"
)

const (
	requestTimeout  = 10 * time.Second
	maxRequestBytes = 256 << 10
	// minDocumentBytes is the smallest GRAPHQL_MAX_DOCUMENT_BYTES accepted:
	// below it the SPA's and playground's own documents would not fit.
	minDocumentBytes = 1 << 10
	depthLimit       = 8
	complexityLimit  = 300
	// Shutdown: /readyz turns 503, serving continues for shutdownDelay, then
	// in-flight requests drain for up to drainTimeout. The chart's preStop sleep
	// and terminationGracePeriodSeconds are sized to cover both.
	shutdownDelay = 3 * time.Second
	drainTimeout  = 20 * time.Second
	readyTimeout  = 3 * time.Second
	// The database check runs in the background (see platform.CachedCheck):
	// one failure is a blip; three in a row (about 15s) take the pod out.
	dbCheckInterval = 5 * time.Second
	dbCheckTimeout  = 2 * time.Second
	dbCheckFailures = 3
	// The telemetry flush runs after the drain: preStop 10s + 3s + 20s + 5s
	// stays inside the chart's 40s termination grace period.
	telemetryFlushTimeout = 5 * time.Second
)

func main() {
	log := platform.NewLogger(os.Stdout, slog.LevelInfo)
	slog.SetDefault(log)
	if err := run(log); err != nil {
		log.Error("api exited", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := loadConfig(platform.NewEnv(nil))
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if len(os.Args) > 1 && os.Args[1] == "migrate" {
		opts, err := migrateFlags(os.Args[2:])
		if err != nil {
			return err
		}
		if err := migrate(ctx, cfg, opts); err != nil {
			return err
		}
		log.Info("migrations applied", "seed", opts.Seed, "createDatabase", opts.CreateDatabase)
		return nil
	}

	// Exporters must outlive the signal context, so the last requests are flushed.
	shutdownTelemetry, err := kitotel.Setup(context.WithoutCancel(ctx), cfg.Telemetry)
	if err != nil {
		return fmt.Errorf("telemetry: %w", err)
	}
	defer kitotel.Flush(log, shutdownTelemetry, telemetryFlushTimeout)
	if _, err := startPartnerKey(ctx, cfg.Partner, log); err != nil {
		return err
	}

	// Listen first and discover the identity provider in the background, so a
	// slow Keycloak start keeps the pod not-ready instead of crash-looping it.
	verifier := oidc.New(authn.Config{
		Issuer: cfg.Issuer, Audience: cfg.Audience, AllowedClients: cfg.AllowedClients, Log: log,
		Transport: kitotel.Transport(nil),
	})
	go func() {
		if err := verifier.Discover(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.ErrorContext(ctx, "oidc discovery stopped", "err", err)
		}
	}()

	store, dbReachable, closeStore, err := openStore(ctx, cfg)
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	defer closeStore()
	// Readiness reads the database check from memory: a probe never queues
	// behind requests for the pool, and a short blip does not pull every pod.
	dbReady := platform.NewCachedCheck(dbReachable, platform.CachedCheckOptions{
		Interval: dbCheckInterval, Timeout: dbCheckTimeout, Threshold: dbCheckFailures, Name: "db", Log: log,
	})
	go dbReady.Run(ctx)
	svc, err := newServices(store, paymentGateway(cfg.Payments, log), openEvents(ctx, store, log), log)
	if err != nil {
		return err
	}

	var draining platform.Draining
	readiness := map[string]platform.Check{"jwks": verifier.Ready, "db": dbReady.Check, "draining": draining.Check}
	persistedQueries, err := persistedQueryPolicy(cfg)
	if err != nil {
		return err
	}
	gql := graphql.NewHandler(svc, graphql.Options{
		Introspection: cfg.Introspection, DepthLimit: depthLimit, ComplexityLimit: complexityLimit, MaxDocumentBytes: cfg.MaxDocumentBytes, Log: log,
		Telemetry:     otelgql.New(otelgql.Options{ClientErrorCodes: graphql.ClientErrorCodes()}),
		Persisted:     persistedQueries,
		Subscriptions: &graphql.SubscriptionOptions{Authenticator: verifier, AllowedOrigins: cfg.CORSOrigins},
	})
	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           newRouter(cfg, log, verifier, readiness, gql),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       requestTimeout,
		WriteTimeout:      requestTimeout + time.Second,
		IdleTimeout:       60 * time.Second,
	}
	log.Info("api listening", "addr", srv.Addr, "env", cfg.Env, "store", cfg.Store, "playground", cfg.Playground, "introspection", cfg.Introspection, "issuer", cfg.Issuer, "audience", cfg.Audience, "telemetry", cfg.Telemetry.Endpoint != "", "payments", cfg.Payments.Enabled)
	return platform.Serve(ctx, srv, platform.Shutdown{OnStart: draining.Begin, Delay: shutdownDelay, Timeout: drainTimeout})
}

// newServices wires the use cases to the store and the other outbound
// adapters; payments may be nil (off).
func newServices(store repositories, payments app.PaymentGateway, events app.Events, log *slog.Logger) (*app.Services, error) {
	return app.NewServices(app.Deps{
		Participants: store, Plans: store, Providers: store, Claims: store, Bookings: store, SupportRequests: store,
		Relationships: store,
		Payments:      payments,
		Events:        events,
		Auditor:       audit.New(log),
		Clock:         time.Now,
		NextID:        func() string { return uuid.Must(uuid.NewV7()).String() }, // time-ordered, unique across replicas
	})
}
