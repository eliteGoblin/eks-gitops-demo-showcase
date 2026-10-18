// Package httpmw is the shared middleware chain. Assemble it in this order:
//
//	[ErrorFormat] → RequestID → tracing (platform/otel) → Recover → AccessLog →
//	SecurityHeaders/CORS → Limit (body + timeout) → RateLimit → Authenticate (protected routes only)
//
// The request id exists before anything logs. Recover sits inside tracing, so
// a panic is a 500 like any other: it reaches the request metrics and the
// span, and its log line carries the trace and request ids (net/http itself
// keeps a panic from crashing the process). Rate limiting runs before
// authentication so a flood of bad tokens never reaches the JWKS verifier.
// ErrorFormat, when a service uses one, comes first so every error is written
// in its envelope.
package httpmw

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/eliteGoblin/eks-gitops-demo/internal/platform"
)

// Recover turns a panic into a 500 and logs it with its stack.
func Recover(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				rec := recover()
				if rec == nil {
					return
				}
				if rec == http.ErrAbortHandler { //nolint:errorlint // sentinel compared by identity, as net/http does
					panic(rec)
				}
				log.ErrorContext(r.Context(), "panic", "panic", rec, "stack", string(debug.Stack()))
				platform.WriteError(w, r, http.StatusInternalServerError, platform.CodeInternal, "internal error")
			}()
			next.ServeHTTP(w, r)
		})
	}
}

const requestIDHeader = "X-Request-ID"

// ErrorFormat makes every error written for the request (by this chain or the
// handlers) use format. Mount it outermost so Recover's 500 uses it too.
func ErrorFormat(format platform.ErrorFormat) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(platform.WithErrorFormat(r.Context(), format)))
		})
	}
}

// RequestID reuses a sane incoming X-Request-ID or creates one, stores it in
// the context and echoes it in the response.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(requestIDHeader)
		if !validRequestID(id) {
			id = newRequestID()
		}
		w.Header().Set(requestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(platform.WithRequestID(r.Context(), id)))
	})
}

func validRequestID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, c := range id {
		if !safeIDChar(c) {
			return false
		}
	}
	return true
}

func safeIDChar(c rune) bool {
	return c == '-' || c == '_' || c == '.' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func newRequestID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += n
	return n, err
}

func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// AccessLog writes one structured line per request. "route" is the matched
// route template (e.g. /payments/{id}), bounded per route, so it is the field
// to aggregate or label metrics on; "path" is the raw, unbounded URL path.
// "client_ip" is what clientIP returns (pass the rate limiter's KeyFunc, so the
// line shows the address the request was limited on); "remote" is the TCP peer.
func AccessLog(log *slog.Logger, clientIP KeyFunc) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w}
			panicked := true
			// Deferred, so a request that panics is logged too, with the 500
			// Recover (outside this middleware) answers it with.
			defer func() {
				status := rec.status
				switch {
				case panicked:
					status = http.StatusInternalServerError
				case status == 0:
					status = http.StatusOK
				}
				log.InfoContext(r.Context(), "http",
					"method", r.Method, "route", RoutePattern(r), "path", r.URL.Path, "status", status, "bytes", rec.bytes,
					"duration_ms", time.Since(start).Milliseconds(), "client_ip", clientIP(r), "remote", r.RemoteAddr)
			}()
			next.ServeHTTP(rec, r)
			panicked = false
		})
	}
}

// UnmatchedRoute is RoutePattern's value when no route matched.
const UnmatchedRoute = "unmatched"

// RoutePattern returns the matched route template, or UnmatchedRoute. Read it
// after the router has run (e.g. after next.ServeHTTP in a middleware).
func RoutePattern(r *http.Request) string {
	if rctx := chi.RouteContext(r.Context()); rctx != nil {
		if p := rctx.RoutePattern(); p != "" {
			return p
		}
	}
	return UnmatchedRoute
}

// DefaultContentSecurityPolicy is the policy of every response that does not
// set its own: nothing may load, frame it or change its base URL. JSON and
// other API responses need nothing more; an HTML page replaces it with a
// policy that lists exactly what the page loads.
const DefaultContentSecurityPolicy = "default-src 'none'; frame-ancestors 'none'; base-uri 'none'"

// SecurityHeaders sets the headers every response should carry.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Cache-Control", "no-store")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", DefaultContentSecurityPolicy)
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=()")
		if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}

// CORS allows cross-origin calls only from the listed origins. Same-origin
// requests (the SPA and the playground) need no CORS headers at all.
func CORS(allowed []string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin == "" {
				next.ServeHTTP(w, r)
				return
			}
			h := w.Header()
			h.Add("Vary", "Origin") // the response differs by Origin, allowed or not
			if !slices.Contains(allowed, origin) {
				next.ServeHTTP(w, r)
				return
			}
			h.Set("Access-Control-Allow-Origin", origin)
			if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
				h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
				h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Request-ID")
				h.Set("Access-Control-Max-Age", "600")
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// Limit caps the request body and gives the handler a deadline. WebSocket
// upgrades get no deadline: the connection outlives the request and bounds
// itself (message size limit, keepalive, token lifetime).
func Limit(maxBytes int64, timeout time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if IsWebSocketUpgrade(r) {
				next.ServeHTTP(w, r)
				return
			}
			if r.ContentLength > maxBytes {
				platform.WriteError(w, r, http.StatusRequestEntityTooLarge, platform.CodeBadUserInput, "request body too large")
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
			ctx, cancel := context.WithTimeout(r.Context(), timeout)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// WebSocketPath is the one path the kit serves WebSocket upgrades on: the
// GraphQL endpoint, for subscriptions.
const WebSocketPath = "/graphql"

// IsWebSocketUpgrade reports whether r is the handshake that opens the
// subscription WebSocket: a GET to WebSocketPath asking to upgrade. Only
// that handshake is exempt from the request deadline and the request
// metrics; an Upgrade header on any other request exempts nothing.
func IsWebSocketUpgrade(r *http.Request) bool {
	return r.Method == http.MethodGet && r.URL.Path == WebSocketPath && strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

// Authenticator verifies a bearer token and returns a context carrying the
// caller's identity. The OIDC adapter implements it.
type Authenticator interface {
	Authenticate(ctx context.Context, rawToken string) (context.Context, error)
}

// Authenticate rejects requests without a valid bearer token with a 401.
// Mount it only on protected routes; public routes skip it.
func Authenticate(a Authenticator, log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok || strings.TrimSpace(raw) == "" {
				unauthorized(w, r, "missing bearer token")
				return
			}
			ctx, err := a.Authenticate(r.Context(), strings.TrimSpace(raw))
			if err != nil {
				log.InfoContext(r.Context(), "token rejected", "err", err)
				unauthorized(w, r, "invalid token")
				return
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func unauthorized(w http.ResponseWriter, r *http.Request, msg string) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="demo-api"`)
	platform.WriteError(w, r, http.StatusUnauthorized, platform.CodeUnauthenticated, msg)
}

// retryAfter formats a Retry-After value in whole seconds (at least 1).
func retryAfter(d time.Duration) string {
	s := int(d.Round(time.Second) / time.Second)
	if s < 1 {
		s = 1
	}
	return strconv.Itoa(s)
}
