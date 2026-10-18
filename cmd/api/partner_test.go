package main

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfigPartnerKey(t *testing.T) {
	https := "https://id.example/realms/demo"
	cfg, err := loadConfig(envOf(map[string]string{"OIDC_ISSUER": https}))
	if err != nil || cfg.Partner.enabled() {
		t.Fatalf("the partner key must be off by default: %v %v", cfg.Partner.enabled(), err)
	}
	cfg, err = loadConfig(envOf(map[string]string{"OIDC_ISSUER": https, "PARTNER_API_KEY": "k-env", "PARTNER_KEY_FILE": "/var/run/secrets/partner/api_key"}))
	if err != nil || cfg.Partner.APIKey != "k-env" || cfg.Partner.KeyFile != "/var/run/secrets/partner/api_key" {
		t.Fatalf("partner = %+v, %v", cfg.Partner.LogValue(), err)
	}
	var logs bytes.Buffer
	slog.New(slog.NewJSONHandler(&logs, nil)).Info("config", "partner", cfg.Partner)
	if strings.Contains(logs.String(), "k-env") || !strings.Contains(logs.String(), `"apiKey":true`) {
		t.Fatalf("logging the partner config must not reveal the key: %s", logs.String())
	}
}

// The partner key is delivered as an environment variable and as a file; a
// broken mount stops startup instead of running without the key.
func TestStartPartnerKey(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "api_key")
	if err := os.WriteFile(file, []byte("k-file"), 0o600); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	key, err := startPartnerKey(ctx, partnerConfig{APIKey: "k-env", KeyFile: file}, log)
	if err != nil || key == nil || key.Current() != "k-file" {
		t.Fatalf("startPartnerKey = %v, %v", key, err)
	}
	for _, want := range []string{`"source":"env"`, `"source":"file"`} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("missing %s in %s", want, logs.String())
		}
	}
	if strings.Contains(logs.String(), "k-env") || strings.Contains(logs.String(), "k-file") {
		t.Fatalf("a key value reached the log: %s", logs.String())
	}
	if _, err := startPartnerKey(ctx, partnerConfig{KeyFile: filepath.Join(dir, "missing")}, log); err == nil {
		t.Fatal("a missing key file must stop startup")
	}
	if key, err := startPartnerKey(ctx, partnerConfig{}, log); err != nil || key != nil {
		t.Fatalf("off: got %v, %v", key, err)
	}
}
