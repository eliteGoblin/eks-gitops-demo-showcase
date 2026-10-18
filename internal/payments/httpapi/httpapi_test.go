package httpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/eliteGoblin/eks-gitops-demo/internal/payments"
	"github.com/eliteGoblin/eks-gitops-demo/internal/payments/httpapi"
	"github.com/eliteGoblin/eks-gitops-demo/internal/payments/memstore"
	"github.com/eliteGoblin/eks-gitops-demo/internal/platform"
)

// newServer mounts the routes behind a stub that plays the auth middleware:
// it stores the caller named in the X-Test-Client header.
func newServer(t *testing.T) *httptest.Server {
	t.Helper()
	return newServerLogging(t, slog.New(slog.DiscardHandler))
}

func newServerLogging(t *testing.T, log *slog.Logger) *httptest.Server {
	t.Helper()
	var seq atomic.Int64
	svc := payments.NewService(memstore.New(), time.Now, func() string { return fmt.Sprintf("pay-%d", seq.Add(1)) })
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := platform.WithErrorFormat(req.Context(), platform.RESTErrors)
			if client := req.Header.Get("X-Test-Client"); client != "" {
				perms := []payments.Permission{payments.PermWrite}
				if req.Header.Get("X-Test-ReadOnly") != "" {
					perms = []payments.Permission{payments.PermRead}
				}
				ctx = payments.WithCaller(ctx, payments.Caller{ClientID: client, Permissions: perms})
			}
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	})
	httpapi.Mount(r, svc, log)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

type call struct {
	method, path, body, key, client string
	readOnly                        bool
}

type result struct {
	status int
	header http.Header
	body   map[string]any
}

func do(t *testing.T, srv *httptest.Server, c call) result {
	t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), c.method, srv.URL+c.path, strings.NewReader(c.body))
	req.Header.Set("Content-Type", "application/json")
	if c.key != "" {
		req.Header.Set("Idempotency-Key", c.key)
	}
	if c.client != "" {
		req.Header.Set("X-Test-Client", c.client)
	}
	if c.readOnly {
		req.Header.Set("X-Test-ReadOnly", "1")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return result{status: resp.StatusCode, header: resp.Header, body: body}
}

const okBody = `{"reference":"C101","payee":"PR1","amountCents":12050,"currency":"AUD"}`

func TestCreateReplayAndGet(t *testing.T) {
	srv := newServer(t)
	created := do(t, srv, call{method: http.MethodPost, path: "/payments", body: okBody, key: "k1", client: "demo-api-svc"})
	if created.status != http.StatusCreated || created.body["status"] != "ACCEPTED" || created.body["amountCents"] != float64(12050) {
		t.Fatalf("create: %d %v", created.status, created.body)
	}
	id, _ := created.body["id"].(string)
	if loc := created.header.Get("Location"); loc != "/payments/"+id {
		t.Fatalf("Location = %q", loc)
	}

	replay := do(t, srv, call{method: http.MethodPost, path: "/payments", body: okBody, key: "k1", client: "demo-api-svc"})
	if replay.status != http.StatusOK || replay.header.Get("Idempotent-Replayed") != "true" || replay.body["id"] != id {
		t.Fatalf("replay: %d %v %v", replay.status, replay.header, replay.body)
	}

	got := do(t, srv, call{method: http.MethodGet, path: "/payments/" + id, client: "demo-api-svc", readOnly: true})
	if got.status != http.StatusOK || got.body["id"] != id || got.body["reference"] != "C101" {
		t.Fatalf("get: %d %v", got.status, got.body)
	}
}

// Every recorded payment is logged with the client that asked for it (the
// token's azp), and whether it was a replay.
func TestPaymentsAreLoggedWithTheirClient(t *testing.T) {
	var buf safeBuffer
	srv := newServerLogging(t, slog.New(slog.NewJSONHandler(&buf, nil)))
	do(t, srv, call{method: http.MethodPost, path: "/payments", body: okBody, key: "k1", client: "demo-api-svc"})
	do(t, srv, call{method: http.MethodPost, path: "/payments", body: okBody, key: "k1", client: "demo-api-svc"})
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("want one log line per payment request, got %q", lines)
	}
	for i, replayed := range []string{"false", "true"} {
		for _, want := range []string{`"msg":"payment recorded"`, `"client":"demo-api-svc"`, `"reference":"C101"`, `"idempotency_key":"k1"`, `"replayed":` + replayed} {
			if !strings.Contains(lines[i], want) {
				t.Errorf("log line %d lacks %s: %s", i, want, lines[i])
			}
		}
	}
}

type safeBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *safeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *safeBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestErrorResponses(t *testing.T) {
	srv := newServer(t)
	do(t, srv, call{method: http.MethodPost, path: "/payments", body: okBody, key: "used", client: "demo-api-svc"})
	tests := []struct {
		name   string
		call   call
		status int
		code   string
	}{
		{"no caller", call{method: http.MethodPost, path: "/payments", body: okBody, key: "k"}, http.StatusForbidden, platform.CodeForbidden},
		{"read-only caller creating", call{method: http.MethodPost, path: "/payments", body: okBody, key: "k", client: "c", readOnly: true}, http.StatusForbidden, platform.CodeForbidden},
		{"missing idempotency key", call{method: http.MethodPost, path: "/payments", body: okBody, client: "c"}, http.StatusBadRequest, platform.CodeBadUserInput},
		{"invalid JSON", call{method: http.MethodPost, path: "/payments", body: `{"amountCents":`, key: "k", client: "c"}, http.StatusBadRequest, platform.CodeBadUserInput},
		{"unknown field", call{method: http.MethodPost, path: "/payments", body: `{"reference":"C1","payee":"PR1","amountCents":1,"currency":"AUD","extra":1}`, key: "k", client: "c"}, http.StatusBadRequest, platform.CodeBadUserInput},
		{"invalid amount", call{method: http.MethodPost, path: "/payments", body: `{"reference":"C1","payee":"PR1","amountCents":0,"currency":"AUD"}`, key: "k", client: "c"}, http.StatusBadRequest, platform.CodeBadUserInput},
		{"key reused with another body", call{method: http.MethodPost, path: "/payments", body: `{"reference":"C999","payee":"PR1","amountCents":1,"currency":"AUD"}`, key: "used", client: "demo-api-svc"}, http.StatusConflict, httpapi.CodeIdempotencyKeyReused},
		{"unknown payment", call{method: http.MethodGet, path: "/payments/pay-404", client: "c"}, http.StatusNotFound, platform.CodeNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := do(t, srv, tc.call)
			errBody, _ := res.body["error"].(map[string]any)
			if res.status != tc.status || errBody["code"] != tc.code {
				t.Fatalf("got %d %v, want %d %s", res.status, res.body, tc.status, tc.code)
			}
		})
	}
}

type brokenService struct{}

func (brokenService) Create(context.Context, string, payments.Request) (payments.Payment, bool, error) {
	return payments.Payment{}, false, errors.New("store: connection reset by 10.0.3.7")
}

func (brokenService) Get(context.Context, string) (payments.Payment, error) {
	return payments.Payment{}, errors.New("store: connection reset by 10.0.3.7")
}

func TestUnexpectedErrorsAreMasked500s(t *testing.T) {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(platform.WithErrorFormat(req.Context(), platform.RESTErrors)))
		})
	})
	httpapi.Mount(r, brokenService{}, slog.New(slog.DiscardHandler))
	srv := httptest.NewServer(r)
	defer srv.Close()
	res := do(t, srv, call{method: http.MethodGet, path: "/payments/pay-1"})
	errBody, _ := res.body["error"].(map[string]any)
	if res.status != http.StatusInternalServerError || errBody["code"] != platform.CodeInternal || errBody["message"] != "internal error" {
		t.Fatalf("got %d %v", res.status, res.body)
	}
}

type fullService struct{ brokenService }

func (fullService) Create(context.Context, string, payments.Request) (payments.Payment, bool, error) {
	return payments.Payment{}, false, payments.ErrStoreFull
}

// A full store is a temporary condition: 503, so the caller retries with the same key.
func TestFullStoreIsA503(t *testing.T) {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(platform.WithErrorFormat(req.Context(), platform.RESTErrors)))
		})
	})
	httpapi.Mount(r, fullService{}, slog.New(slog.DiscardHandler))
	srv := httptest.NewServer(r)
	defer srv.Close()
	res := do(t, srv, call{method: http.MethodPost, path: "/payments", key: "k1",
		body: `{"reference":"C1","payee":"PR1","amountCents":100,"currency":"AUD"}`})
	errBody, _ := res.body["error"].(map[string]any)
	if res.status != http.StatusServiceUnavailable || errBody["code"] != platform.CodeUnavailable {
		t.Fatalf("got %d %v", res.status, res.body)
	}
}
