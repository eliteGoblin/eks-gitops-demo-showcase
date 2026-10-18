// Package paymentsclient is demo-api's outbound adapter to payments-svc. It
// implements app.PaymentGateway with a cached client-credentials token, a
// timeout per attempt, retries with capped backoff (safe only because every
// request carries an idempotency key) and a circuit breaker.
package paymentsclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sony/gobreaker/v2"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"

	"github.com/eliteGoblin/eks-gitops-demo/internal/app"
)

// Config describes how to reach and authenticate to payments-svc.
type Config struct {
	BaseURL      string // e.g. http://payments-svc.demo.svc:8083
	TokenURL     string // Keycloak token endpoint
	ClientID     string // demo-api-svc
	ClientSecret string

	Timeout          time.Duration // per attempt (default 3s)
	MaxAttempts      int           // including the first (default 3)
	BaseBackoff      time.Duration // first retry delay, doubled each time (default 100ms)
	MaxBackoff       time.Duration // cap on one retry delay, and on a Retry-After we wait for (default 2s)
	FailureThreshold uint32        // consecutive failures that open the breaker (default 5)
	OpenTimeout      time.Duration // how long the breaker stays open (default 30s)

	// Transport carries the token and payment requests (e.g. an instrumented
	// one that propagates the trace); nil uses http.DefaultTransport.
	Transport http.RoundTripper
	// Log receives breaker state changes and token refreshes; nil discards them.
	Log *slog.Logger
}

func (c Config) withDefaults() Config {
	if c.Timeout <= 0 {
		c.Timeout = 3 * time.Second
	}
	if c.MaxAttempts < 1 {
		c.MaxAttempts = 3
	}
	if c.BaseBackoff <= 0 {
		c.BaseBackoff = 100 * time.Millisecond
	}
	if c.MaxBackoff <= 0 {
		c.MaxBackoff = 2 * time.Second
	}
	if c.FailureThreshold == 0 {
		c.FailureThreshold = 5
	}
	if c.OpenTimeout <= 0 {
		c.OpenTimeout = 30 * time.Second
	}
	if c.Log == nil {
		c.Log = slog.New(slog.DiscardHandler)
	}
	return c
}

// maxResponseBytes bounds how much of a response body is read.
const maxResponseBytes = 64 << 10

// Client implements app.PaymentGateway.
type Client struct {
	cfg     Config
	tokens  *tokens
	http    *http.Client
	breaker *gobreaker.CircuitBreaker[app.PaymentReceipt]
}

var _ app.PaymentGateway = (*Client)(nil)

// New returns a client. The service token is fetched on first use and reused
// until shortly before it expires.
func New(cfg Config) *Client {
	cfg = cfg.withDefaults()
	tokenHTTP := &http.Client{Timeout: cfg.Timeout, Transport: cfg.Transport}
	cc := clientcredentials.Config{ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret, TokenURL: cfg.TokenURL}
	ts := newTokens(cc, context.WithValue(context.Background(), oauth2.HTTPClient, tokenHTTP))
	breaker := gobreaker.NewCircuitBreaker[app.PaymentReceipt](gobreaker.Settings{
		Name:        "payments-svc",
		Timeout:     cfg.OpenTimeout,
		ReadyToTrip: func(c gobreaker.Counts) bool { return c.ConsecutiveFailures >= cfg.FailureThreshold },
		// Only an unavailable processor counts: refused connections, 5xx and
		// attempts that ran out of time (a hung service is as broken as a
		// down one). A rejected request, or the caller giving up, says nothing
		// about its health either way: such results are excluded, so they can
		// neither open the breaker nor close it from half-open (a cancelled
		// probe frees the probe slot instead).
		IsExcluded: func(err error) bool {
			if errors.Is(err, app.ErrPaymentUnavailable) {
				return false
			}
			return errors.Is(err, app.ErrPaymentRejected) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
		},
		IsSuccessful: func(err error) bool { return err == nil },
		OnStateChange: func(name string, from, to gobreaker.State) {
			cfg.Log.Warn("circuit breaker state changed", "breaker", name, "from", from.String(), "to", to.String())
		},
	})
	return &Client{
		cfg:    cfg,
		tokens: ts,
		// No client-wide timeout: each attempt gets its own deadline (see send),
		// so an attempt that runs out of time is told apart from a caller who
		// gave up.
		http:    &http.Client{Transport: &oauth2.Transport{Source: ts, Base: cfg.Transport}},
		breaker: breaker,
	}
}

type paymentBody struct {
	Reference   string `json:"reference"`
	Payee       string `json:"payee"`
	AmountCents int64  `json:"amountCents"`
	Currency    string `json:"currency"`
}

// RequestPayment asks payments-svc to pay req. Errors wrap
// app.ErrPaymentRejected (do not retry) or app.ErrPaymentUnavailable (retry
// later with the same idempotency key), or are the context's error.
func (c *Client) RequestPayment(ctx context.Context, req app.PaymentRequest) (app.PaymentReceipt, error) {
	if strings.TrimSpace(req.IdempotencyKey) == "" {
		return app.PaymentReceipt{}, fmt.Errorf("%w: an idempotency key is required", app.ErrPaymentRejected)
	}
	body, err := json.Marshal(paymentBody{Reference: req.Reference, Payee: req.Payee, AmountCents: req.AmountCents, Currency: req.Currency})
	if err != nil {
		return app.PaymentReceipt{}, err
	}
	receipt, err := c.breaker.Execute(func() (app.PaymentReceipt, error) {
		return c.sendWithRetries(ctx, body, req.IdempotencyKey)
	})
	if errors.Is(err, gobreaker.ErrOpenState) || errors.Is(err, gobreaker.ErrTooManyRequests) {
		return app.PaymentReceipt{}, fmt.Errorf("%w: circuit breaker open", app.ErrPaymentUnavailable)
	}
	return receipt, err
}

// sendWithRetries retries an unavailable processor with capped, jittered
// backoff, or after the Retry-After it asked for when that is short enough.
func (c *Client) sendWithRetries(ctx context.Context, body []byte, key string) (app.PaymentReceipt, error) {
	var lastErr error
	for attempt := 1; attempt <= c.cfg.MaxAttempts; attempt++ {
		receipt, err := c.sendAuthenticated(ctx, body, key)
		if err == nil || !errors.Is(err, app.ErrPaymentUnavailable) {
			return receipt, err
		}
		lastErr = err
		if attempt == c.cfg.MaxAttempts {
			break
		}
		wait, ok := c.retryDelay(err, attempt)
		if !ok {
			break // the processor asked for a longer pause than we are willing to hold the request
		}
		if err := sleep(ctx, wait); err != nil {
			return app.PaymentReceipt{}, err
		}
	}
	return app.PaymentReceipt{}, lastErr
}

// retryDelay is the server's Retry-After when it sent one (and it fits the
// cap), otherwise the capped exponential backoff.
func (c *Client) retryDelay(err error, attempt int) (time.Duration, bool) {
	var busy *unavailableError
	if errors.As(err, &busy) && busy.retryAfter > 0 {
		return busy.retryAfter, busy.retryAfter <= c.cfg.MaxBackoff
	}
	return backoff(c.cfg.BaseBackoff, c.cfg.MaxBackoff, attempt), true
}

// sendAuthenticated sends one attempt. A 401 means payments-svc rejected the
// cached token (e.g. Keycloak rotated its keys or the client secret): the
// token is dropped, a new one fetched, and the request sent once more.
func (c *Client) sendAuthenticated(ctx context.Context, body []byte, key string) (app.PaymentReceipt, error) {
	receipt, err := c.send(ctx, body, key)
	if !errors.Is(err, errTokenRejected) {
		return receipt, err
	}
	c.cfg.Log.InfoContext(ctx, "payments-svc rejected the service token; fetching a new one")
	c.tokens.reset()
	return c.send(ctx, body, key)
}

// errTokenRejected is a 401 from payments-svc.
var errTokenRejected = fmt.Errorf("%w: service token rejected (HTTP 401)", app.ErrPaymentRejected)

// unavailableError is a 429 or 5xx; retryAfter is the server's hint, if any.
type unavailableError struct {
	status     int
	retryAfter time.Duration
}

func (e *unavailableError) Error() string {
	return fmt.Sprintf("%v: HTTP %d", app.ErrPaymentUnavailable, e.status)
}

func (e *unavailableError) Unwrap() error { return app.ErrPaymentUnavailable }

// send makes one attempt, bounded by the attempt timeout. Running out of that
// time is the processor's fault (ErrPaymentUnavailable); the caller's own
// context ending is not, and comes back as the context's error.
func (c *Client) send(ctx context.Context, body []byte, key string) (app.PaymentReceipt, error) {
	attempt, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(attempt, http.MethodPost, c.cfg.BaseURL+"/payments", bytes.NewReader(body))
	if err != nil {
		return app.PaymentReceipt{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Idempotency-Key", key)
	resp, err := c.http.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return app.PaymentReceipt{}, ctx.Err()
		}
		return app.PaymentReceipt{}, fmt.Errorf("%w: %w", app.ErrPaymentUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	limited := io.LimitReader(resp.Body, maxResponseBytes)

	switch {
	case resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated:
		var out struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		}
		if err := json.NewDecoder(limited).Decode(&out); err != nil {
			return app.PaymentReceipt{}, fmt.Errorf("%w: unreadable response: %w", app.ErrPaymentUnavailable, err)
		}
		return app.PaymentReceipt{PaymentID: out.ID, Status: out.Status, Replayed: resp.Header.Get("Idempotent-Replayed") == "true"}, nil
	case resp.StatusCode == http.StatusUnauthorized:
		_, _ = io.Copy(io.Discard, limited)
		return app.PaymentReceipt{}, errTokenRejected
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= http.StatusInternalServerError:
		_, _ = io.Copy(io.Discard, limited)
		return app.PaymentReceipt{}, &unavailableError{status: resp.StatusCode, retryAfter: parseRetryAfter(resp.Header.Get("Retry-After"))}
	default:
		_, _ = io.Copy(io.Discard, limited)
		return app.PaymentReceipt{}, fmt.Errorf("%w: HTTP %d", app.ErrPaymentRejected, resp.StatusCode)
	}
}

// parseRetryAfter reads a Retry-After given in seconds; dates and junk are ignored.
func parseRetryAfter(v string) time.Duration {
	secs, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || secs <= 0 {
		return 0
	}
	return time.Duration(secs) * time.Second
}

// backoff doubles base per attempt up to maxDelay, with jitter, so retries
// from many pods don't arrive in lockstep.
func backoff(base, maxDelay time.Duration, attempt int) time.Duration {
	d := maxDelay
	if attempt < 31 { // beyond this the shift overflows; the cap applies anyway
		if shifted := base << (attempt - 1); shifted > 0 && shifted < maxDelay {
			d = shifted
		}
	}
	return d/2 + rand.N(d/2+1) //nolint:gosec // jitter, not a security value
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// tokens is a client-credentials token source whose cached token can be
// dropped, so a token the resource server rejects is replaced at once.
type tokens struct {
	mu  sync.Mutex
	cc  clientcredentials.Config
	ctx context.Context // carries the token endpoint's HTTP client
	src oauth2.TokenSource
}

func newTokens(cc clientcredentials.Config, ctx context.Context) *tokens {
	t := &tokens{cc: cc, ctx: ctx}
	t.reset()
	return t
}

// Token returns the cached token, fetching a new one shortly before expiry.
func (t *tokens) Token() (*oauth2.Token, error) {
	t.mu.Lock()
	src := t.src
	t.mu.Unlock()
	return src.Token()
}

func (t *tokens) reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.src = oauth2.ReuseTokenSource(nil, t.cc.TokenSource(t.ctx))
}
