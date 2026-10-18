// Package platform is the shared service kit every service in this repo uses:
// configuration, logging, the HTTP server lifecycle, health endpoints and the
// error envelope. Middleware lives in platform/httpmw.
package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/trace"
)

// Env reads configuration from environment variables and collects every
// problem, so a misconfigured service reports all of them at once.
type Env struct {
	lookup func(string) (string, bool)
	errs   []error
}

// NewEnv reads from the process environment; pass a lookup func in tests.
func NewEnv(lookup func(string) (string, bool)) *Env {
	if lookup == nil {
		lookup = os.LookupEnv
	}
	return &Env{lookup: lookup}
}

// String returns the variable or def when it is unset or empty.
func (e *Env) String(key, def string) string {
	if v, ok := e.lookup(key); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

// Required returns the variable and records an error when it is missing.
func (e *Env) Required(key string) string {
	v := e.String(key, "")
	if v == "" {
		e.errs = append(e.errs, fmt.Errorf("%s is required", key))
	}
	return v
}

// URL returns the variable (or def) and records an error unless it is an absolute http(s) URL.
func (e *Env) URL(key, def string) string {
	v := e.String(key, def)
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		e.errs = append(e.errs, fmt.Errorf("%s must be an absolute http(s) URL, got %q", key, v))
	}
	return v
}

// OneOf returns the variable (or def) and records an error unless it is allowed.
func (e *Env) OneOf(key, def string, allowed ...string) string {
	v := e.String(key, def)
	for _, a := range allowed {
		if v == a {
			return v
		}
	}
	e.errs = append(e.errs, fmt.Errorf("%s must be one of %v, got %q", key, allowed, v))
	return v
}

// Bool returns the variable parsed as a boolean, or def when unset.
func (e *Env) Bool(key string, def bool) bool {
	v := e.String(key, "")
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		e.errs = append(e.errs, fmt.Errorf("%s must be a boolean, got %q", key, v))
	}
	return b
}

// Float returns the variable parsed as a positive number, or def when unset.
func (e *Env) Float(key string, def float64) float64 {
	v := e.String(key, "")
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f <= 0 {
		e.errs = append(e.errs, fmt.Errorf("%s must be a positive number, got %q", key, v))
	}
	return f
}

// Int returns the variable parsed as a positive integer, or def when unset.
func (e *Env) Int(key string, def int) int {
	v := e.String(key, "")
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		e.errs = append(e.errs, fmt.Errorf("%s must be a positive integer, got %q", key, v))
	}
	return n
}

// Errorf records a problem that spans several variables (e.g. "https issuer required in prod").
func (e *Env) Errorf(format string, args ...any) {
	e.errs = append(e.errs, fmt.Errorf(format, args...))
}

// List splits a comma-separated variable, dropping empty items.
func (e *Env) List(key string) []string {
	var out []string
	for _, item := range strings.Split(e.String(key, ""), ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

// Err returns every configuration problem found so far.
func (e *Env) Err() error { return errors.Join(e.errs...) }

type requestIDKey struct{}

// WithRequestID stores the request id in the context.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// RequestIDFrom returns the request id, or "" outside a request.
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// NewLogger returns a JSON slog logger that adds request_id and, inside a
// span, trace_id/span_id (plus the X-Ray form of the trace id) from the
// context to every record logged with a *Context method.
func NewLogger(w io.Writer, level slog.Level) *slog.Logger {
	return slog.New(contextHandler{slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level})})
}

type contextHandler struct{ slog.Handler }

func (h contextHandler) Handle(ctx context.Context, r slog.Record) error {
	if id := RequestIDFrom(ctx); id != "" {
		r.AddAttrs(slog.String("request_id", id))
	}
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		tid := sc.TraceID().String()
		r.AddAttrs(
			slog.String("trace_id", tid),
			slog.String("span_id", sc.SpanID().String()),
			slog.String("xray_trace_id", "1-"+tid[:8]+"-"+tid[8:]),
		)
	}
	return h.Handler.Handle(ctx, r)
}

func (h contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return contextHandler{h.Handler.WithAttrs(attrs)}
}

func (h contextHandler) WithGroup(name string) slog.Handler {
	return contextHandler{h.Handler.WithGroup(name)}
}

// Error codes used in the envelope and in GraphQL extensions.code.
const (
	CodeUnauthenticated = "UNAUTHENTICATED"
	CodeForbidden       = "FORBIDDEN"
	CodeBadUserInput    = "BAD_USER_INPUT"
	CodeRateLimited     = "RATE_LIMITED"
	CodeInternal        = "INTERNAL"
	CodeNotFound        = "NOT_FOUND"
	CodeConflict        = "CONFLICT"
	CodeUnavailable     = "UNAVAILABLE"
	// CodeCancelled: the client cancelled the request (it went away); a
	// client outcome, never counted as a server failure.
	CodeCancelled = "CANCELLED"
)

// ErrorFormat selects the shape of the error envelope a service writes.
type ErrorFormat int

const (
	// GraphQLErrors writes {"errors":[{"message", "extensions":{"code","request_id"}}]},
	// so a GraphQL client reads errors raised before the GraphQL handler runs.
	GraphQLErrors ErrorFormat = iota
	// RESTErrors writes {"error":{"code","message","request_id"}} for REST services.
	RESTErrors
)

type errorFormatKey struct{}

// WithErrorFormat stores the service's error envelope shape in the context
// (httpmw.ErrorFormat does this for every request).
func WithErrorFormat(ctx context.Context, f ErrorFormat) context.Context {
	return context.WithValue(ctx, errorFormatKey{}, f)
}

func errorFormatFrom(ctx context.Context) ErrorFormat {
	f, _ := ctx.Value(errorFormatKey{}).(ErrorFormat)
	return f
}

// WriteError writes the error envelope in the request's ErrorFormat
// (GraphQL shape unless the service chose RESTErrors).
func WriteError(w http.ResponseWriter, r *http.Request, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	id := RequestIDFrom(r.Context())
	if errorFormatFrom(r.Context()) == RESTErrors {
		body := map[string]string{"code": code, "message": msg}
		if id != "" {
			body["request_id"] = id
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"error": body})
		return
	}
	ext := map[string]any{"code": code}
	if id != "" {
		ext["request_id"] = id
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]any{{"message": msg, "extensions": ext}}})
}

// Healthz is the liveness probe: the process is up and serving.
func Healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// Check is one readiness dependency check.
type Check func(ctx context.Context) error

// Readyz is the readiness probe: every check must pass within the timeout.
// The endpoint is public, so a failure names the check and the details go to
// the log only.
func Readyz(timeout time.Duration, checks map[string]Check, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		var failed []string
		for name, check := range checks {
			if err := check(ctx); err != nil {
				failed = append(failed, name)
				log.WarnContext(ctx, "readiness check failed", "check", name, "err", err)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if len(failed) > 0 {
			slices.Sort(failed)
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "unavailable", "failed": failed})
			return
		}
		_, _ = w.Write([]byte(`{"status":"ready"}`))
	}
}

// errDraining is reported by Draining.Check once shutdown has begun.
var errDraining = errors.New("shutting down")

// Draining is a readiness check that starts failing when shutdown begins, so
// the load balancer stops routing to the pod before it stops serving.
type Draining struct{ on atomic.Bool }

// Begin marks the service as shutting down.
func (d *Draining) Begin() { d.on.Store(true) }

// Check fails once Begin has been called.
func (d *Draining) Check(context.Context) error {
	if d.on.Load() {
		return errDraining
	}
	return nil
}

// Shutdown configures how Serve stops after its context is cancelled.
type Shutdown struct {
	// OnStart runs first, e.g. Draining.Begin so /readyz reports 503.
	OnStart func()
	// Delay keeps serving while the load balancer deregisters the pod.
	Delay time.Duration
	// Timeout bounds the drain of in-flight requests; then connections are closed.
	Timeout time.Duration
}

// Serve runs srv until ctx is cancelled (SIGTERM from Kubernetes), then shuts
// down in three steps: report not-ready, keep serving for Delay, and drain
// in-flight requests within Timeout (closing whatever is left after that).
func Serve(ctx context.Context, srv *http.Server, sd Shutdown) error {
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}
	if sd.OnStart != nil {
		sd.OnStart()
	}
	time.Sleep(sd.Delay)
	drainCtx, cancel := context.WithTimeout(context.Background(), sd.Timeout)
	defer cancel()
	if err := srv.Shutdown(drainCtx); err != nil {
		_ = srv.Close()
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	if err := <-errCh; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
