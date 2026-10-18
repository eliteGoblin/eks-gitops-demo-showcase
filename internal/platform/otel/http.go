package otel

import (
	"net/http"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/eliteGoblin/eks-gitops-demo/internal/platform/httpmw"
)

const routeKey = attribute.Key("http.route")

var unrecordedPaths = map[string]bool{"/healthz": true, "/readyz": true, "/metrics": true}

// recorded skips health probes and WebSocket upgrades. A WebSocket stays open
// for minutes, so its upgrade request would put the whole connection's
// lifetime into the latency histogram; subscriptions are measured per
// operation by the GraphQL extension instead.
func recorded(r *http.Request) bool {
	if unrecordedPaths[r.URL.Path] {
		return false
	}
	return !httpmw.IsWebSocketUpgrade(r)
}

// HTTPOption configures HTTPMiddleware.
type HTTPOption func(*[]otelhttp.Option)

// AsPublicEndpoint makes every request start a new trace, keeping the
// caller's trace context only as a link. Use it on internet-facing services:
// an outside caller can then neither join its own trace nor force sampling.
func AsPublicEndpoint() HTTPOption {
	return func(opts *[]otelhttp.Option) {
		*opts = append(*opts, otelhttp.WithPublicEndpointFn(func(*http.Request) bool { return true }))
	}
}

// HTTPMiddleware is the tracing and metrics slot of the kit's chain (after
// RequestID, before AccessLog). Per request it records a server span named
// "METHOD /route/template", the http.server.request.duration histogram
// labelled with the route template (never the raw path; a request no route
// matched keeps an empty route) and status code, and auth.failures for 401
// and 403 responses. Health probes and WebSocket upgrades are not recorded.
func HTTPMiddleware(service string, opts ...HTTPOption) func(http.Handler) http.Handler {
	otelOpts := []otelhttp.Option{
		// server.address is the service, never the client-supplied Host; the
		// ":0" stops otelhttp from taking server.port from the Host header,
		// where any caller could otherwise mint one series per port.
		otelhttp.WithServerName(service + ":0"),
		otelhttp.WithFilter(recorded),
		otelhttp.WithSpanNameFormatter(spanName),
	}
	for _, opt := range opts {
		opt(&otelOpts)
	}
	instrument := otelhttp.NewMiddleware(service, otelOpts...)
	authFailures := AuthFailures(Meter())
	return func(next http.Handler) http.Handler {
		return instrument(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sw := &statusWriter{ResponseWriter: w}
			next.ServeHTTP(sw, r)
			labelRoute(r)
			if sw.status == http.StatusUnauthorized || sw.status == http.StatusForbidden {
				authFailures.Add(r.Context(), 1, metric.WithAttributes(AuthFailureCode(sw.status)))
			}
		}))
	}
}

// labelRoute adds the matched route template to the span and the duration
// metric. It runs after the handler: the router has matched by then.
func labelRoute(r *http.Request) {
	route := httpmw.RoutePattern(r)
	if route == httpmw.UnmatchedRoute {
		return
	}
	if l, ok := otelhttp.LabelerFromContext(r.Context()); ok {
		l.Add(routeKey.String(route))
	}
	span := trace.SpanFromContext(r.Context())
	span.SetName(r.Method + " " + route)
	span.SetAttributes(routeKey.String(route))
}

// spanName names the span after the matched route once the router has run;
// otelhttp asks again after the handler when the request carries a pattern.
func spanName(_ string, r *http.Request) string {
	if route := httpmw.RoutePattern(r); route != httpmw.UnmatchedRoute {
		return r.Method + " " + route
	}
	return r.Method
}

// statusWriter remembers the response status for auth.failures.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// Transport instruments an outbound HTTP client: a client span per request,
// named after the remote host (X-Ray names the remote node after it), and the
// trace context injected into the request headers, so the callee's spans join
// the caller's trace.
func Transport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return otelhttp.NewTransport(base,
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string { return r.URL.Host }))
}
