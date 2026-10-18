// Command payments serves payments-svc: an internal REST mock of a payment
// processor, called only by demo-api with a client-credentials token.
// This file is the composition root: config → adapters → use cases → server.
package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/eliteGoblin/eks-gitops-demo/internal/payments"
	"github.com/eliteGoblin/eks-gitops-demo/internal/payments/identity"
	"github.com/eliteGoblin/eks-gitops-demo/internal/payments/memstore"
	"github.com/eliteGoblin/eks-gitops-demo/internal/platform"
	"github.com/eliteGoblin/eks-gitops-demo/internal/platform/authn"
	kitotel "github.com/eliteGoblin/eks-gitops-demo/internal/platform/otel"
)

const (
	requestTimeout  = 5 * time.Second
	maxRequestBytes = 16 << 10
	shutdownDelay   = 3 * time.Second
	drainTimeout    = 20 * time.Second
	readyTimeout    = 3 * time.Second
	// After the drain: preStop 10s + 3s + 20s + 5s fits the 40s grace period.
	telemetryFlushTimeout = 5 * time.Second
)

func main() {
	log := platform.NewLogger(os.Stdout, slog.LevelInfo)
	slog.SetDefault(log)
	if err := run(log); err != nil {
		log.Error("payments exited", "err", err)
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

	shutdownTelemetry, err := kitotel.Setup(context.WithoutCancel(ctx), cfg.Telemetry)
	if err != nil {
		return fmt.Errorf("telemetry: %w", err)
	}
	defer kitotel.Flush(log, shutdownTelemetry, telemetryFlushTimeout)

	verifier := authn.New(authn.Config{
		Issuer: cfg.Issuer, Audience: cfg.Audience, AllowedClients: cfg.AllowedClients, Log: log,
		Transport: kitotel.Transport(nil),
	})
	go func() {
		if err := verifier.Discover(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.ErrorContext(ctx, "oidc discovery stopped", "err", err)
		}
	}()

	svc := payments.NewService(memstore.New(), func() time.Time { return time.Now().UTC() }, newPaymentID)
	var draining platform.Draining
	readiness := map[string]platform.Check{"jwks": verifier.Ready, "draining": draining.Check}
	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           newRouter(cfg, log, verifier.Authenticator(identity.Mapper(cfg.Audience)), readiness, svc),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       requestTimeout,
		WriteTimeout:      requestTimeout + time.Second,
		IdleTimeout:       60 * time.Second,
	}
	log.Info("payments listening", "addr", srv.Addr, "env", cfg.Env, "issuer", cfg.Issuer, "audience", cfg.Audience, "allowed_clients", cfg.AllowedClients)
	return platform.Serve(ctx, srv, platform.Shutdown{OnStart: draining.Begin, Delay: shutdownDelay, Timeout: drainTimeout})
}

// newPaymentID returns a random, unguessable id such as "pay_3f9c…".
func newPaymentID() string {
	return "pay_" + rand.Text()
}
