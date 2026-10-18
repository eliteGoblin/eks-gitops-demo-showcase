package paymentsclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eliteGoblin/eks-gitops-demo/internal/app"
)

// fakes plays Keycloak's token endpoint and payments-svc.
type fakes struct {
	srv          *httptest.Server
	tokenCalls   atomic.Int64
	paymentCalls atomic.Int64
	keys         chan string
	traceparents chan string
	respond      func(n int64, w http.ResponseWriter) // n = payment call number, from 1
}

func newFakes(t *testing.T, respond func(n int64, w http.ResponseWriter)) *fakes {
	t.Helper()
	f := &fakes{keys: make(chan string, 100), traceparents: make(chan string, 100), respond: respond}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		f.tokenCalls.Add(1)
		if r.FormValue("grant_type") != "client_credentials" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"svc-token","token_type":"Bearer","expires_in":300}`))
	})
	mux.HandleFunc("POST /payments", func(w http.ResponseWriter, r *http.Request) {
		n := f.paymentCalls.Add(1)
		if r.Header.Get("Authorization") != "Bearer svc-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		f.keys <- r.Header.Get("Idempotency-Key")
		f.traceparents <- r.Header.Get("Traceparent")
		f.respond(n, w)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func accepted(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{"id": "pay_1", "status": "ACCEPTED"})
}

func (f *fakes) client(t *testing.T, tweak func(*Config)) *Client {
	t.Helper()
	cfg := Config{
		BaseURL: f.srv.URL, TokenURL: f.srv.URL + "/token", ClientID: "demo-api-svc", ClientSecret: "dev-secret",
		Timeout: time.Second, MaxAttempts: 3, BaseBackoff: time.Millisecond, FailureThreshold: 3, OpenTimeout: time.Minute,
	}
	if tweak != nil {
		tweak(&cfg)
	}
	return New(cfg)
}

var req = app.PaymentRequest{IdempotencyKey: "C101", Reference: "C101", Payee: "PR1", AmountCents: 12050, Currency: "AUD"}

func TestRequestPaymentCachesTheServiceTokenAndSendsHeaders(t *testing.T) {
	f := newFakes(t, func(_ int64, w http.ResponseWriter) { accepted(w) })
	tr := &markingTransport{}
	c := f.client(t, func(cfg *Config) { cfg.Transport = tr })
	for range 3 {
		got, err := c.RequestPayment(context.Background(), req)
		if err != nil || got.PaymentID != "pay_1" || got.Status != "ACCEPTED" || got.Replayed {
			t.Fatalf("receipt %+v, err %v", got, err)
		}
	}
	if n := f.tokenCalls.Load(); n != 1 {
		t.Fatalf("token endpoint called %d times; the token must be cached until it expires", n)
	}
	if k, tp := <-f.keys, <-f.traceparents; k != "C101" || tp != "00-abc-def-01" {
		t.Fatalf("Idempotency-Key %q, traceparent %q", k, tp)
	}
	if n := tr.calls.Load(); n != 4 {
		t.Fatalf("the transport carried %d requests, want 4 (the token and three payments)", n)
	}
}

// markingTransport stands in for an instrumented transport: it adds trace
// context to every request it carries.
type markingTransport struct{ calls atomic.Int64 }

func (m *markingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	m.calls.Add(1)
	r = r.Clone(r.Context())
	r.Header.Set("Traceparent", "00-abc-def-01")
	return http.DefaultTransport.RoundTrip(r)
}

func TestReplayedPaymentIsReported(t *testing.T) {
	f := newFakes(t, func(_ int64, w http.ResponseWriter) {
		w.Header().Set("Idempotent-Replayed", "true")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "pay_1", "status": "ACCEPTED"})
	})
	got, err := f.client(t, nil).RequestPayment(context.Background(), req)
	if err != nil || !got.Replayed {
		t.Fatalf("receipt %+v, err %v", got, err)
	}
}

func TestTransientFailuresAreRetriedWithTheSameKey(t *testing.T) {
	f := newFakes(t, func(n int64, w http.ResponseWriter) {
		if n < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		accepted(w)
	})
	got, err := f.client(t, nil).RequestPayment(context.Background(), req)
	if err != nil || got.PaymentID != "pay_1" {
		t.Fatalf("receipt %+v, err %v", got, err)
	}
	if f.paymentCalls.Load() != 3 {
		t.Fatalf("payment calls = %d, want 3 (two 503s, then success)", f.paymentCalls.Load())
	}
	for range 3 {
		if k := <-f.keys; k != "C101" {
			t.Fatalf("every attempt must reuse the idempotency key, got %q", k)
		}
	}
}

func TestRejectionsAreNotRetriedAndDoNotTripTheBreaker(t *testing.T) {
	f := newFakes(t, func(_ int64, w http.ResponseWriter) { w.WriteHeader(http.StatusBadRequest) })
	c := f.client(t, nil)
	for range 5 { // more than FailureThreshold
		if _, err := c.RequestPayment(context.Background(), req); !errors.Is(err, app.ErrPaymentRejected) {
			t.Fatalf("400 must be ErrPaymentRejected, got %v", err)
		}
	}
	if f.paymentCalls.Load() != 5 {
		t.Fatalf("payment calls = %d, want 5: a 4xx is not retried and must not open the breaker", f.paymentCalls.Load())
	}
}

func TestBreakerOpensWhenThePaymentServiceIsDown(t *testing.T) {
	f := newFakes(t, func(_ int64, w http.ResponseWriter) { w.WriteHeader(http.StatusBadGateway) })
	c := f.client(t, func(cfg *Config) { cfg.MaxAttempts = 1 })
	for range 3 { // FailureThreshold
		if _, err := c.RequestPayment(context.Background(), req); !errors.Is(err, app.ErrPaymentUnavailable) {
			t.Fatalf("5xx must be ErrPaymentUnavailable, got %v", err)
		}
	}
	before := f.paymentCalls.Load()
	start := time.Now()
	if _, err := c.RequestPayment(context.Background(), req); !errors.Is(err, app.ErrPaymentUnavailable) {
		t.Fatalf("an open breaker must fail fast with ErrPaymentUnavailable, got %v", err)
	}
	if f.paymentCalls.Load() != before || time.Since(start) > 100*time.Millisecond {
		t.Fatal("an open breaker must not call payments-svc")
	}
}

func TestUnreachableServiceIsUnavailable(t *testing.T) {
	f := newFakes(t, func(_ int64, w http.ResponseWriter) { accepted(w) })
	c := f.client(t, func(cfg *Config) { cfg.BaseURL = "http://127.0.0.1:1" })
	if _, err := c.RequestPayment(context.Background(), req); !errors.Is(err, app.ErrPaymentUnavailable) {
		t.Fatalf("want ErrPaymentUnavailable, got %v", err)
	}
}

func TestRequestWithoutIdempotencyKeyIsRefused(t *testing.T) {
	f := newFakes(t, func(_ int64, w http.ResponseWriter) { accepted(w) })
	noKey := req
	noKey.IdempotencyKey = ""
	if _, err := f.client(t, nil).RequestPayment(context.Background(), noKey); !errors.Is(err, app.ErrPaymentRejected) {
		t.Fatalf("want ErrPaymentRejected, got %v", err)
	}
	if f.paymentCalls.Load() != 0 {
		t.Fatal("a request that cannot be retried safely must not be sent")
	}
}

func TestCancelledContextStopsRetrying(t *testing.T) {
	f := newFakes(t, func(_ int64, w http.ResponseWriter) { w.WriteHeader(http.StatusServiceUnavailable) })
	c := f.client(t, func(cfg *Config) { cfg.BaseBackoff = time.Hour })
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := c.RequestPayment(ctx, req); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want the context error, got %v", err)
	}
}

// A cancelled call says nothing about payments-svc's health: it must not
// count against the breaker (closed) nor close it from half-open.
func TestCallerCancellationDoesNotMoveTheBreaker(t *testing.T) {
	var down atomic.Bool
	down.Store(true)
	f := newFakes(t, func(_ int64, w http.ResponseWriter) {
		if down.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		accepted(w)
	})
	c := f.client(t, func(cfg *Config) { cfg.MaxAttempts = 1; cfg.OpenTimeout = 50 * time.Millisecond })
	for range 3 { // FailureThreshold: open
		_, _ = c.RequestPayment(context.Background(), req)
	}
	time.Sleep(60 * time.Millisecond) // now half-open: the next call is the single probe
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.RequestPayment(cancelled, req); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled probe must return the context error, got %v", err)
	}
	// The cancelled probe must have freed the half-open slot without closing
	// the breaker: the next call is a real probe, and its failure re-opens it.
	before := f.paymentCalls.Load()
	if _, err := c.RequestPayment(context.Background(), req); !errors.Is(err, app.ErrPaymentUnavailable) {
		t.Fatalf("want a real probe that fails, got %v", err)
	}
	if f.paymentCalls.Load() != before+1 {
		t.Fatal("the probe after a cancelled one must reach payments-svc")
	}
	if _, err := c.RequestPayment(context.Background(), req); err == nil || !strings.Contains(err.Error(), "circuit breaker open") {
		t.Fatalf("a failed probe must re-open the breaker, got %v", err)
	}
}

// After the open timeout one probe goes through; when the service has
// recovered, it closes the breaker and traffic flows again.
func TestBreakerRecoversThroughHalfOpen(t *testing.T) {
	var down atomic.Bool
	down.Store(true)
	f := newFakes(t, func(_ int64, w http.ResponseWriter) {
		if down.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		accepted(w)
	})
	c := f.client(t, func(cfg *Config) { cfg.MaxAttempts = 1; cfg.OpenTimeout = 50 * time.Millisecond })
	for range 3 {
		_, _ = c.RequestPayment(context.Background(), req)
	}
	if _, err := c.RequestPayment(context.Background(), req); err == nil || !strings.Contains(err.Error(), "circuit breaker open") {
		t.Fatalf("breaker must be open, got %v", err)
	}
	down.Store(false)
	time.Sleep(60 * time.Millisecond)
	for i := range 3 {
		if got, err := c.RequestPayment(context.Background(), req); err != nil || got.PaymentID != "pay_1" {
			t.Fatalf("call %d after recovery: %+v %v", i, got, err)
		}
	}
}

// A 401 means payments-svc rejected the cached token (rotated keys, rotated
// secret): drop it, fetch a new one and resend once.
func TestRejectedTokenIsRefreshedOnce(t *testing.T) {
	var tokenCalls, payCalls atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, _ *http.Request) {
		n := tokenCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"access_token":"tok-%d","token_type":"Bearer","expires_in":300}`, n)
	})
	mux.HandleFunc("POST /payments", func(w http.ResponseWriter, r *http.Request) {
		payCalls.Add(1)
		if r.Header.Get("Authorization") != "Bearer tok-2" { // only the refreshed token is valid
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		accepted(w)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := New(Config{BaseURL: srv.URL, TokenURL: srv.URL + "/token", ClientID: "demo-api-svc", ClientSecret: "s", Timeout: time.Second})
	got, err := c.RequestPayment(context.Background(), req)
	if err != nil || got.PaymentID != "pay_1" {
		t.Fatalf("receipt %+v, err %v", got, err)
	}
	if tokenCalls.Load() != 2 || payCalls.Load() != 2 {
		t.Fatalf("token calls %d, payment calls %d; want one refresh and one resend", tokenCalls.Load(), payCalls.Load())
	}
}

// A token that keeps being rejected is a rejection, not an outage: no loop,
// no breaker movement.
func TestPersistentlyRejectedTokenIsARejection(t *testing.T) {
	f := newFakes(t, func(_ int64, w http.ResponseWriter) { accepted(w) })
	c := f.client(t, func(cfg *Config) { cfg.ClientSecret = "wrong" })
	// The fake accepts only "svc-token"; point payments at a server that always says 401.
	always401 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) }))
	t.Cleanup(always401.Close)
	c.cfg.BaseURL = always401.URL
	for range 5 {
		if _, err := c.RequestPayment(context.Background(), req); !errors.Is(err, app.ErrPaymentRejected) {
			t.Fatalf("want ErrPaymentRejected, got %v", err)
		}
	}
	if f.tokenCalls.Load() != 6 { // first fetch + one refresh per call
		t.Fatalf("token calls = %d, want 6", f.tokenCalls.Load())
	}
}

// Retry-After is honoured when it fits the cap; a longer pause ends the retries.
func TestRetryAfterIsHonoured(t *testing.T) {
	f := newFakes(t, func(n int64, w http.ResponseWriter) {
		if n == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		accepted(w)
	})
	c := f.client(t, func(cfg *Config) { cfg.MaxBackoff = 2 * time.Second })
	start := time.Now()
	if _, err := c.RequestPayment(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if waited := time.Since(start); waited < 900*time.Millisecond {
		t.Fatalf("waited %v; a 429 with Retry-After: 1 must pause about a second", waited)
	}

	g := newFakes(t, func(_ int64, w http.ResponseWriter) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	slow := g.client(t, func(cfg *Config) { cfg.MaxBackoff = time.Second })
	start = time.Now()
	if _, err := slow.RequestPayment(context.Background(), req); !errors.Is(err, app.ErrPaymentUnavailable) {
		t.Fatalf("want ErrPaymentUnavailable, got %v", err)
	}
	if g.paymentCalls.Load() != 1 || time.Since(start) > 500*time.Millisecond {
		t.Fatalf("a Retry-After beyond the cap must stop retrying at once (calls %d)", g.paymentCalls.Load())
	}
}

func TestBackoffIsCappedAndNeverPanics(t *testing.T) {
	for _, attempt := range []int{1, 2, 10, 40, 1000} {
		d := backoff(100*time.Millisecond, time.Second, attempt)
		if d <= 0 || d > time.Second {
			t.Fatalf("attempt %d: backoff %v outside (0, 1s]", attempt, d)
		}
	}
}

func TestOversizedResponseIsNotReadWhole(t *testing.T) {
	f := newFakes(t, func(_ int64, w http.ResponseWriter) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"`))
		_, _ = w.Write(bytes.Repeat([]byte("a"), 2*maxResponseBytes))
		_, _ = w.Write([]byte(`","status":"ACCEPTED"}`))
	})
	if _, err := f.client(t, func(cfg *Config) { cfg.MaxAttempts = 1 }).RequestPayment(context.Background(), req); !errors.Is(err, app.ErrPaymentUnavailable) {
		t.Fatalf("an oversized body must be refused as unreadable, got %v", err)
	}
}

// A payments-svc that accepts connections but never answers is as broken as
// one that is down: each attempt that runs out of time counts against the
// breaker, which then fails fast instead of holding every approval for the
// full timeout.
func TestTimeoutsOpenTheBreaker(t *testing.T) {
	f := newFakes(t, func(_ int64, w http.ResponseWriter) {
		time.Sleep(300 * time.Millisecond) // longer than the attempt timeout below
		accepted(w)
	})
	c := f.client(t, func(cfg *Config) { cfg.MaxAttempts = 1; cfg.Timeout = 50 * time.Millisecond })
	for i := range 3 { // FailureThreshold
		if _, err := c.RequestPayment(context.Background(), req); !errors.Is(err, app.ErrPaymentUnavailable) {
			t.Fatalf("timed-out attempt %d must be ErrPaymentUnavailable, got %v", i+1, err)
		}
	}
	before := f.paymentCalls.Load()
	start := time.Now()
	_, err := c.RequestPayment(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "circuit breaker open") {
		t.Fatalf("after 3 timeouts the breaker must be open, got %v", err)
	}
	if f.paymentCalls.Load() != before || time.Since(start) > 40*time.Millisecond {
		t.Fatal("an open breaker must fail fast without calling payments-svc")
	}
}
