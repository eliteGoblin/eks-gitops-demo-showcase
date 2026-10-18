package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/eliteGoblin/eks-gitops-demo/internal/payments"
	"github.com/eliteGoblin/eks-gitops-demo/internal/payments/identity"
	"github.com/eliteGoblin/eks-gitops-demo/internal/payments/memstore"
	"github.com/eliteGoblin/eks-gitops-demo/internal/platform"
	"github.com/eliteGoblin/eks-gitops-demo/internal/platform/authn"
	"github.com/eliteGoblin/eks-gitops-demo/internal/platform/authn/authntest"
)

func newTestServer(t *testing.T, idp *authntest.IdP) *httptest.Server {
	t.Helper()
	cfg := config{Issuer: idp.URL, Audience: "payments-svc", AllowedClients: []string{"demo-api-svc"}, RateRPS: 100, RateBurst: 100}
	log := slog.New(slog.DiscardHandler)
	verifier := authn.New(authn.Config{Issuer: cfg.Issuer, Audience: cfg.Audience, AllowedClients: cfg.AllowedClients, Log: log})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := verifier.Discover(ctx); err != nil {
		t.Fatal(err)
	}
	svc := payments.NewService(memstore.New(), time.Now, func() string { return "pay-1" })
	readiness := map[string]platform.Check{"jwks": verifier.Ready}
	srv := httptest.NewServer(newRouter(cfg, log, verifier.Authenticator(identity.Mapper(cfg.Audience)), readiness, svc))
	t.Cleanup(srv.Close)
	return srv
}

func post(t *testing.T, srv *httptest.Server, token string) (status int, body string) {
	t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/payments",
		strings.NewReader(`{"reference":"C101","payee":"PR1","amountCents":100,"currency":"AUD"}`))
	req.Header.Set("Idempotency-Key", "k1")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestOnlyTheAllowedServiceWithPaymentsWriteMayPay(t *testing.T) {
	idp := authntest.NewIdP(t)
	srv := newTestServer(t, idp)
	withRole := func(c map[string]any) map[string]any {
		c["resource_access"] = map[string]any{"payments-svc": map[string]any{"roles": []string{"payments:write"}}}
		return c
	}

	tests := []struct {
		name   string
		token  string
		status int
	}{
		{"service token from demo-api-svc", idp.Sign(t, withRole(idp.AccessToken("payments-svc", "demo-api-svc"))), http.StatusCreated},
		{"no token", "", http.StatusUnauthorized},
		{"user token for demo-api (wrong audience)", idp.Sign(t, withRole(idp.AccessToken("demo-api", "spa"))), http.StatusUnauthorized},
		{"token for payments-svc from another client (azp)", idp.Sign(t, withRole(idp.AccessToken("payments-svc", "other-svc"))), http.StatusUnauthorized},
		{"allowed client without payments:write", idp.Sign(t, idp.AccessToken("payments-svc", "demo-api-svc")), http.StatusForbidden},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			status, body := post(t, srv, tc.token)
			if status != tc.status {
				t.Fatalf("status %d, want %d: %s", status, tc.status, body)
			}
			if tc.status >= 400 && !strings.Contains(body, `"error":{`) {
				t.Fatalf("errors must use the REST envelope, got %s", body)
			}
		})
	}
}

func TestHealthAndReadiness(t *testing.T) {
	srv := newTestServer(t, authntest.NewIdP(t))
	for _, path := range []string{"/healthz", "/readyz"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s = %d", path, resp.StatusCode)
		}
	}
}

func TestLoadConfig(t *testing.T) {
	env := func(kv map[string]string) *platform.Env {
		return platform.NewEnv(func(k string) (string, bool) { v, ok := kv[k]; return v, ok })
	}
	cfg, err := loadConfig(env(map[string]string{"OIDC_ISSUER": "https://auth.example/realms/demo"}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Audience != "payments-svc" || cfg.Port != "8083" || len(cfg.AllowedClients) != 1 || cfg.AllowedClients[0] != "demo-api-svc" {
		t.Fatalf("defaults = %+v", cfg)
	}
	if cfg.Telemetry.Endpoint != "" || cfg.Telemetry.ServiceName != "payments-svc" {
		t.Fatalf("telemetry must be off by default and named payments-svc, got %+v", cfg.Telemetry)
	}
	if _, err := loadConfig(env(map[string]string{"OIDC_ISSUER": "http://keycloak:8080/realms/demo"})); err == nil {
		t.Fatal("prod (the default) must refuse an http issuer")
	}
	if _, err := loadConfig(env(map[string]string{"ENV": "dev", "OIDC_ISSUER": "http://keycloak:8080/realms/demo"})); err != nil {
		t.Fatalf("dev may use an http issuer: %v", err)
	}
	if _, err := loadConfig(env(map[string]string{})); err == nil {
		t.Fatal("OIDC_ISSUER is required")
	}
}

// Unknown routes and wrong methods answer in the REST error envelope too.
func TestUnknownRoutesUseTheErrorEnvelope(t *testing.T) {
	srv := newTestServer(t, authntest.NewIdP(t))
	for _, tc := range []struct {
		method, path string
		status       int
		code         string
	}{
		{http.MethodGet, "/nope", http.StatusNotFound, "NOT_FOUND"},
		{http.MethodDelete, "/healthz", http.StatusMethodNotAllowed, "BAD_USER_INPUT"},
	} {
		req, _ := http.NewRequestWithContext(context.Background(), tc.method, srv.URL+tc.path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != tc.status || !strings.Contains(string(b), `"code":"`+tc.code+`"`) {
			t.Fatalf("%s %s: %d %s", tc.method, tc.path, resp.StatusCode, b)
		}
	}
}
