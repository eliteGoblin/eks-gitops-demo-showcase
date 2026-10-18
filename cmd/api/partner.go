package main

import (
	"context"
	"fmt"
	"log/slog"

	kitotel "github.com/eliteGoblin/eks-gitops-demo/internal/platform/otel"
	"github.com/eliteGoblin/eks-gitops-demo/internal/platform/secretfile"
)

// partnerKeyName names the partner API key in logs and metrics.
const partnerKeyName = "partner-api-key"

// partnerConfig delivers the partner API key (Secrets Manager → external-
// secrets → Kubernetes Secret) in both supported ways, so the demo shows both
// rotation behaviours side by side.
type partnerConfig struct {
	APIKey  string // PARTNER_API_KEY: fixed until the next restart (Reloader rolls the pods)
	KeyFile string // PARTNER_KEY_FILE: a mounted Secret file, re-read without a restart
}

func (p partnerConfig) enabled() bool { return p.APIKey != "" || p.KeyFile != "" }

// LogValue keeps the key out of any log line that includes the config.
func (p partnerConfig) LogValue() slog.Value {
	return slog.GroupValue(slog.Bool("apiKey", p.APIKey != ""), slog.String("keyFile", p.KeyFile))
}

// startPartnerKey reports both deliveries and keeps the file one current.
// Nothing calls the partner yet: this demonstrates delivery and rotation. A
// real partner client would read the returned file's Current() for every call,
// so a rotation takes effect without a restart. Returns nil when no file is
// configured.
func startPartnerKey(ctx context.Context, cfg partnerConfig, log *slog.Logger) (*secretfile.File, error) {
	if !cfg.enabled() {
		return nil, nil
	}
	recorder, err := secretfile.NewRecorder(kitotel.Meter())
	if err != nil {
		return nil, err
	}
	opts := secretfile.Options{Name: partnerKeyName, Log: log, Recorder: recorder}
	if cfg.APIKey != "" {
		secretfile.RecordEnv(ctx, cfg.APIKey, opts)
	}
	if cfg.KeyFile == "" {
		return nil, nil
	}
	file, err := secretfile.Open(ctx, cfg.KeyFile, opts)
	if err != nil {
		return nil, fmt.Errorf("partner key: %w", err)
	}
	go file.Run(ctx)
	return file, nil
}
