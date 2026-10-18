package main

import (
	"log/slog"
	"time"

	"github.com/eliteGoblin/eks-gitops-demo/internal/adapter/paymentsclient"
	"github.com/eliteGoblin/eks-gitops-demo/internal/app"
	kitotel "github.com/eliteGoblin/eks-gitops-demo/internal/platform/otel"
)

// A payment runs inside the approval request: two short attempts and a capped
// retry pause keep the worst case (about 5s) well within requestTimeout.
const (
	paymentTimeout     = 2 * time.Second
	paymentAttempts    = 2
	paymentMaxBackoff  = time.Second
	paymentBaseBackoff = 100 * time.Millisecond
)

// paymentGateway returns the payments-svc client, or nil when payments are
// off (approvals then leave claims unpaid).
func paymentGateway(cfg paymentsConfig, log *slog.Logger) app.PaymentGateway {
	if !cfg.Enabled {
		return nil
	}
	return paymentsclient.New(paymentsclient.Config{
		BaseURL: cfg.URL, TokenURL: cfg.TokenURL, ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret,
		Timeout: paymentTimeout, MaxAttempts: paymentAttempts, BaseBackoff: paymentBaseBackoff, MaxBackoff: paymentMaxBackoff,
		Transport: kitotel.Transport(nil), // the trace continues into payments-svc
		Log:       log,
	})
}
