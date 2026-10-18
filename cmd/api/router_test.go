package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5"

	"github.com/eliteGoblin/eks-gitops-demo/internal/adapter/events"
	"github.com/eliteGoblin/eks-gitops-demo/internal/adapter/graphql"
	"github.com/eliteGoblin/eks-gitops-demo/internal/adapter/memory"
	"github.com/eliteGoblin/eks-gitops-demo/internal/app"
	"github.com/eliteGoblin/eks-gitops-demo/internal/platform"
)

type denyAll struct{}

func (denyAll) Authenticate(context.Context, string) (context.Context, error) {
	return nil, errors.New("no")
}

func envOf(vars map[string]string) *platform.Env {
	return platform.NewEnv(func(k string) (string, bool) { v, ok := vars[k]; return v, ok })
}

func testRouter(t *testing.T, playground bool) http.Handler {
	t.Helper()
	vars := map[string]string{"OIDC_ISSUER": "https://id.example/realms/demo"}
	if playground {
		vars["PLAYGROUND_ENABLED"] = "true"
	}
	cfg, err := loadConfig(envOf(vars))
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ready := map[string]platform.Check{"jwks": func(context.Context) error { return nil }}
	return newRouter(cfg, log, denyAll{}, ready, http.NotFoundHandler())
}

func get(h http.Handler, method, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

func TestConfigJSONExposesIssuer(t *testing.T) {
	rec := get(testRouter(t, false), http.MethodGet, "/config.json")
	var body struct{ Issuer, ClientID string }
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || rec.Code != 200 {
		t.Fatalf("status %d body %s err %v", rec.Code, rec.Body, err)
	}
	if body.Issuer != "https://id.example/realms/demo" || body.ClientID != "spa" {
		t.Fatalf("unexpected config %+v", body)
	}
}

func TestRoutes(t *testing.T) {
	dev, prod := testRouter(t, true), testRouter(t, false)
	tests := []struct {
		name   string
		h      http.Handler
		method string
		path   string
		status int
		body   string
	}{
		{"spa page", prod, http.MethodGet, "/", 200, "token walkthrough"},
		{"oidc helper", prod, http.MethodGet, "/oidc.js", 200, "createOIDC"},
		{"healthz", prod, http.MethodGet, "/healthz", 200, "ok"},
		{"readyz", prod, http.MethodGet, "/readyz", 200, "ready"},
		{"playground when enabled", dev, http.MethodGet, "/playground", 200, "graphiql"},
		{"no playground by default", prod, http.MethodGet, "/playground", 404, ""},
		{"graphql needs a token", prod, http.MethodPost, "/graphql", 401, "UNAUTHENTICATED"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := get(tc.h, tc.method, tc.path)
			if rec.Code != tc.status || !strings.Contains(strings.ToLower(rec.Body.String()), strings.ToLower(tc.body)) {
				t.Fatalf("got %d %q", rec.Code, rec.Body.String()[:min(rec.Body.Len(), 200)])
			}
			if rec.Header().Get("X-Request-ID") == "" || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Fatal("middleware chain not applied")
			}
		})
	}
}

// allowStaff authenticates any token as agency staff.
type allowStaff struct{}

func (allowStaff) Authenticate(ctx context.Context, _ string) (context.Context, error) {
	return app.WithPrincipal(ctx, app.Principal{Subject: "s", Roles: map[app.Role]bool{app.RoleAgencyStaff: true}}), nil
}

// A WebSocket passes the whole middleware chain (every wrapper must let the
// connection be hijacked) and authenticates in connection_init, not on the
// upgrade request; POST still needs the bearer token.
func TestGraphQLWebSocketThroughTheRouter(t *testing.T) {
	cfg, err := loadConfig(envOf(map[string]string{"OIDC_ISSUER": "https://id.example/realms/demo"}))
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.DiscardHandler)
	st := memory.New()
	memory.Seed(st)
	svc, err := newServices(st, nil, events.NewHub(log), log)
	if err != nil {
		t.Fatal(err)
	}
	gql := graphql.NewHandler(svc, graphql.Options{DepthLimit: depthLimit, ComplexityLimit: complexityLimit, Log: log,
		Subscriptions: &graphql.SubscriptionOptions{Authenticator: allowStaff{}}})
	srv := httptest.NewServer(newRouter(cfg, log, denyAll{}, map[string]platform.Check{}, gql))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	//nolint:bodyclose // websocket.Dial: "You never need to close resp.Body yourself"
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/graphql", &websocket.DialOptions{Subprotocols: []string{"graphql-transport-ws"}})
	if err != nil {
		t.Fatalf("upgrade through the router: %v", err)
	}
	defer func() { _ = conn.CloseNow() }()
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"connection_init","payload":{"Authorization":"Bearer t"}}`)); err != nil {
		t.Fatal(err)
	}
	if _, msg, err := conn.Read(ctx); err != nil || !strings.Contains(string(msg), `"connection_ack"`) {
		t.Fatalf("want connection_ack, got %s %v", msg, err)
	}
	if rec := get(srv.Config.Handler, http.MethodPost, "/graphql"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("POST /graphql without a token: %d", rec.Code)
	}
}

func TestLoadConfigRejectsBadValues(t *testing.T) {
	_, err := loadConfig(envOf(map[string]string{"ENV": "staging", "OIDC_ISSUER": "keycloak", "RATE_LIMIT_BURST": "0.5"}))
	for _, want := range []string{"ENV", "OIDC_ISSUER", "RATE_LIMIT_BURST"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("want a %s error, got %v", want, err)
		}
	}
}

// Forgetting configuration must never expose development features.
func TestLoadConfigIsSafeByDefault(t *testing.T) {
	cfg, err := loadConfig(envOf(map[string]string{"OIDC_ISSUER": "https://id.example/realms/demo"}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Env != "prod" || cfg.Playground || cfg.Introspection {
		t.Fatalf("defaults must be prod with playground and introspection off, got %+v", cfg)
	}
	if cfg.Telemetry.Endpoint != "" || cfg.Telemetry.ServiceName != "demo-api" {
		t.Fatalf("telemetry must be off by default and named demo-api, got %+v", cfg.Telemetry)
	}
}

func TestLoadConfigIssuerScheme(t *testing.T) {
	tests := []struct {
		name string
		vars map[string]string
		ok   bool
	}{
		{"prod refuses an http issuer", map[string]string{"OIDC_ISSUER": "http://id.example/realms/demo"}, false},
		{"prod accepts https", map[string]string{"OIDC_ISSUER": "https://id.example/realms/demo"}, true},
		{"dev accepts http (local Keycloak)", map[string]string{"ENV": "dev", "OIDC_ISSUER": "http://localhost:8080/realms/demo"}, true},
		{"features are explicit opt-ins, even in prod", map[string]string{"OIDC_ISSUER": "https://id.example/realms/demo", "PLAYGROUND_ENABLED": "true", "INTROSPECTION_ENABLED": "true"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadConfig(envOf(tc.vars))
			if (err == nil) != tc.ok {
				t.Fatalf("ok=%v, got err %v", tc.ok, err)
			}
		})
	}
}

func TestLoadConfigStore(t *testing.T) {
	https := "https://id.example/realms/demo"
	cfg, err := loadConfig(envOf(map[string]string{"OIDC_ISSUER": https}))
	if err != nil || cfg.Store != storeMemory {
		t.Fatalf("default store must be memory, got %q %v", cfg.Store, err)
	}
	cfg, err = loadConfig(envOf(map[string]string{"OIDC_ISSUER": https, "STORE": "postgres", "DATABASE_URL": "postgres://db/demo"}))
	if err != nil || cfg.Store != storePostgres || cfg.DatabaseURL != "postgres://db/demo" {
		t.Fatalf("postgres store: %+v %v", cfg, err)
	}
	if _, err := loadConfig(envOf(map[string]string{"OIDC_ISSUER": https, "STORE": "mysql"})); err == nil || !strings.Contains(err.Error(), "STORE") {
		t.Fatalf("unknown store must be rejected: %v", err)
	}
}

func TestLoadConfigPayments(t *testing.T) {
	https := "https://id.example/realms/demo"
	cfg, err := loadConfig(envOf(map[string]string{"OIDC_ISSUER": https}))
	if err != nil || cfg.Payments.Enabled {
		t.Fatalf("payments must be off by default: %+v %v", cfg.Payments, err)
	}
	if _, err := loadConfig(envOf(map[string]string{"OIDC_ISSUER": https, "PAYMENTS_ENABLED": "true"})); err == nil || !strings.Contains(err.Error(), "PAYMENTS_CLIENT_SECRET") {
		t.Fatalf("enabled payments need the client secret: %v", err)
	}
	cfg, err = loadConfig(envOf(map[string]string{"OIDC_ISSUER": https, "PAYMENTS_ENABLED": "true", "PAYMENTS_CLIENT_SECRET": "s",
		"PAYMENTS_URL": "http://payments-svc.demo.svc:8083"}))
	want := paymentsConfig{Enabled: true, URL: "http://payments-svc.demo.svc:8083", TokenURL: https + "/protocol/openid-connect/token", ClientID: "demo-api-svc", ClientSecret: "s"}
	if err != nil || cfg.Payments != want {
		t.Fatalf("payments = %+v, %v; want %+v", cfg.Payments, err, want)
	}
	if _, err := loadConfig(envOf(map[string]string{"OIDC_ISSUER": https, "PAYMENTS_ENABLED": "true", "PAYMENTS_CLIENT_SECRET": "s", "PAYMENTS_URL": "payments-svc"})); err == nil || !strings.Contains(err.Error(), "PAYMENTS_URL") {
		t.Fatalf("a relative PAYMENTS_URL must be rejected: %v", err)
	}
}

func TestLoadConfigClientIPTrust(t *testing.T) {
	https := "https://id.example/realms/demo"
	cfg, err := loadConfig(envOf(map[string]string{"OIDC_ISSUER": https}))
	if err != nil || cfg.TrustProxy || cfg.TrustCFConnectingIP {
		t.Fatalf("no client-IP header may be trusted by default: %+v %v", cfg, err)
	}
	cfg, err = loadConfig(envOf(map[string]string{"OIDC_ISSUER": https, "TRUST_CF_CONNECTING_IP": "true"}))
	if err != nil || !cfg.TrustCFConnectingIP || cfg.TrustProxy {
		t.Fatalf("TRUST_CF_CONNECTING_IP=true: %+v %v", cfg, err)
	}
	_, err = loadConfig(envOf(map[string]string{"OIDC_ISSUER": https, "TRUST_PROXY": "true", "TRUST_CF_CONNECTING_IP": "true"}))
	if err == nil || !strings.Contains(err.Error(), "TRUST_PROXY") || !strings.Contains(err.Error(), "TRUST_CF_CONNECTING_IP") {
		t.Fatalf("trusting both headers at once must be refused: %v", err)
	}
	if _, err := loadConfig(envOf(map[string]string{"OIDC_ISSUER": https, "TRUST_CF_CONNECTING_IP": "on"})); err == nil || !strings.Contains(err.Error(), "TRUST_CF_CONNECTING_IP") {
		t.Fatalf("a non-boolean TRUST_CF_CONNECTING_IP must be refused: %v", err)
	}
}

// The rate limiter keys on the client-IP header the configuration trusts, and
// on nothing else. With TRUST_CF_CONNECTING_IP off (the ALB fallback window),
// a forged CF-Connecting-IP cannot buy a fresh bucket; with it on, a malformed
// one cannot either. The access log records the same client IP.
func TestRateLimitKeysOnTheTrustedClientIPOnly(t *testing.T) {
	tests := []struct {
		name     string
		vars     map[string]string
		cfIPs    []string // CF-Connecting-IP of consecutive requests from one peer
		want     []int
		wantLogs string // the last request's access-log client_ip
	}{
		{"flag off: forged values share the peer's bucket", nil, []string{"198.51.100.7", "198.51.100.8"}, []int{200, 429}, "10.1.1.1"},
		{"flag off behind the ALB: forged values share the X-Forwarded-For bucket", map[string]string{"TRUST_PROXY": "true"}, []string{"198.51.100.7", "198.51.100.8"}, []int{200, 429}, "203.0.113.9"},
		{"flag on: each client address has its own bucket", map[string]string{"TRUST_CF_CONNECTING_IP": "true"}, []string{"198.51.100.7", "198.51.100.8"}, []int{200, 200}, "198.51.100.8"},
		{"flag on: one client address is limited", map[string]string{"TRUST_CF_CONNECTING_IP": "true"}, []string{"198.51.100.7", "198.51.100.7"}, []int{200, 429}, "198.51.100.7"},
		{"flag on: two spellings of one address share a bucket", map[string]string{"TRUST_CF_CONNECTING_IP": "true"}, []string{"2001:DB8::1", "2001:db8:0:0:0:0:0:1"}, []int{200, 429}, "2001:db8::1"},
		{"flag on: malformed values share the peer's bucket", map[string]string{"TRUST_CF_CONNECTING_IP": "true"}, []string{"garbage", "198.51.100.7, 198.51.100.8"}, []int{200, 429}, "10.1.1.1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// One token per bucket, refilled only after ~17 minutes: every 200 is a fresh bucket.
			vars := map[string]string{"OIDC_ISSUER": "https://id.example/realms/demo", "RATE_LIMIT_RPS": "0.001", "RATE_LIMIT_BURST": "1"}
			for k, v := range tc.vars {
				vars[k] = v
			}
			cfg, err := loadConfig(envOf(vars))
			if err != nil {
				t.Fatal(err)
			}
			var logs strings.Builder
			ready := map[string]platform.Check{"jwks": func(context.Context) error { return nil }}
			h := newRouter(cfg, platform.NewLogger(&logs, slog.LevelInfo), denyAll{}, ready, http.NotFoundHandler())
			for i, cfIP := range tc.cfIPs {
				req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
				req.RemoteAddr = "10.1.1.1:443"
				req.Header.Set("X-Forwarded-For", "203.0.113.9")
				req.Header.Set("CF-Connecting-IP", cfIP)
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				if rec.Code != tc.want[i] {
					t.Fatalf("request %d (CF-Connecting-IP %q): status %d, want %d", i+1, cfIP, rec.Code, tc.want[i])
				}
			}
			lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
			if last := lines[len(lines)-1]; !strings.Contains(last, `"client_ip":"`+tc.wantLogs+`"`) {
				t.Fatalf("the access log must show the client IP the limiter keyed on (%s): %s", tc.wantLogs, last)
			}
		})
	}
}

// A database that cannot be reached stops startup with an error (no typed-nil
// store reaches the use cases) and the error does not echo the password.
func TestOpenStoreFailsFast(t *testing.T) {
	cfg := config{Store: storePostgres, DatabaseURL: "postgres://demo:s3cret-PW@127.0.0.1:1/demo?sslmode=disable&connect_timeout=1"}
	repos, ready, closeFn, err := openStore(context.Background(), cfg)
	if err == nil || repos != nil || ready != nil || closeFn != nil {
		t.Fatalf("want only an error, got repos=%v ready=%v close=%v err=%v", repos, ready != nil, closeFn != nil, err)
	}
	if strings.Contains(err.Error(), "s3cret-PW") {
		t.Fatalf("error leaks the password: %v", err)
	}
	if err := migrate(context.Background(), config{Store: storeMemory}, migrateOptions{}); err == nil {
		t.Fatal("migrate must refuse the memory store")
	}
}

func TestMigrateFlags(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want migrateOptions
		ok   bool
	}{
		{nil, migrateOptions{}, true},
		{[]string{"--seed"}, migrateOptions{Seed: true}, true},
		{[]string{"--create-db"}, migrateOptions{CreateDatabase: true}, true},
		{[]string{"--create-db", "--seed"}, migrateOptions{Seed: true, CreateDatabase: true}, true},
		{[]string{"--sead"}, migrateOptions{}, false},
		{[]string{"--seed", "extra"}, migrateOptions{}, false},
	} {
		got, err := migrateFlags(tc.args)
		if (err == nil) != tc.ok || (tc.ok && got != tc.want) {
			t.Errorf("migrateFlags(%q) = %+v, %v; want %+v, ok=%v", tc.args, got, err, tc.want, tc.ok)
		}
	}
}

// `api migrate` creates a missing database only when asked: a typo in the
// database name must fail the Job, not silently create an empty database.
func TestMigrateCreatesTheDatabaseOnlyWhenAsked(t *testing.T) {
	base := os.Getenv("TEST_DATABASE_URL")
	u, err := url.Parse(base)
	if base == "" || err != nil || u.Scheme == "" {
		t.Skip("needs a URL-form TEST_DATABASE_URL")
	}
	name := "migrate_" + strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "_")
	t.Cleanup(func() {
		conn, err := pgx.Connect(context.Background(), base)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close(context.Background()) }()
		_, _ = conn.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})
	u.Path = "/" + name
	cfg := config{Store: storePostgres, DatabaseURL: u.String()}
	ctx := context.Background()
	if err := migrate(ctx, cfg, migrateOptions{}); err == nil {
		t.Fatal("without --create-db a missing database must fail")
	}
	if err := migrate(ctx, cfg, migrateOptions{CreateDatabase: true}); err != nil {
		t.Fatalf("with --create-db: %v", err)
	}
	if err := migrate(ctx, cfg, migrateOptions{}); err != nil {
		t.Fatalf("an existing database migrates without the flag: %v", err)
	}
}
