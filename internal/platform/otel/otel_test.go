package otel

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/eliteGoblin/eks-gitops-demo/internal/platform"
)

func TestSetupWithoutEndpointIsANoop(t *testing.T) {
	shutdown, err := Setup(context.Background(), Config{})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	// Propagators are installed even without an endpoint, so trace headers
	// still flow through the service.
	fields := otel.GetTextMapPropagator().Fields()
	for _, want := range []string{"traceparent", "baggage", "X-Amzn-Trace-Id"} {
		if !slices.Contains(fields, want) {
			t.Errorf("propagator fields %v lack %s", fields, want)
		}
	}
}

func TestSetupRejectsBadConfig(t *testing.T) {
	for name, cfg := range map[string]Config{
		"no service name": {Endpoint: "localhost:4317"},
		"ratio zero":      {Endpoint: "localhost:4317", ServiceName: "x", SampleRatio: 0},
		"ratio above one": {Endpoint: "localhost:4317", ServiceName: "x", SampleRatio: 1.5},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Setup(context.Background(), cfg); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func metricWithUnit(u string) metric.Float64HistogramOption { return metric.WithUnit(u) }

// collect returns every metric the reader has, flattened by scope.
func collect(t *testing.T, reader *sdkmetric.ManualReader) []metricdata.Metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	var out []metricdata.Metrics
	for _, sm := range rm.ScopeMetrics {
		out = append(out, sm.Metrics...)
	}
	return out
}

func find(ms []metricdata.Metrics, name string) (metricdata.Metrics, bool) {
	for _, m := range ms {
		if m.Name == name {
			return m, true
		}
	}
	return metricdata.Metrics{}, false
}

func TestSecondsHistogramsGetDurationBuckets(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := newMeterProvider(resource.Empty(), reader)
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	seconds, _ := mp.Meter("test").Float64Histogram("graphql.operation.duration", metricWithUnit("s"))
	seconds.Record(context.Background(), 0.03)
	bytes, _ := mp.Meter("test").Float64Histogram("payload.size", metricWithUnit("By"))
	bytes.Record(context.Background(), 1)

	ms := collect(t, reader)
	m, ok := find(ms, "graphql.operation.duration")
	if !ok {
		t.Fatal("histogram not collected")
	}
	h := m.Data.(metricdata.Histogram[float64])
	if got := h.DataPoints[0].Bounds; !slices.Equal(got, DurationBuckets) {
		t.Errorf("seconds histogram bounds = %v, want %v", got, DurationBuckets)
	}
	other, _ := find(ms, "payload.size")
	if got := other.Data.(metricdata.Histogram[float64]).DataPoints[0].Bounds; slices.Equal(got, DurationBuckets) {
		t.Errorf("a non-seconds histogram must keep the SDK defaults, got %v", got)
	}
}

func TestHTTPMiddlewareLabelsTheRouteAndSkipsProbes(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := newMeterProvider(resource.Empty(), reader)
	spans := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()); _ = tp.Shutdown(context.Background()) })
	otel.SetMeterProvider(mp)
	otel.SetTracerProvider(tp)

	r := chi.NewRouter()
	r.Use(HTTPMiddleware("demo-api"))
	r.Get("/payments/{id}", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	r.Get("/private", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) })

	for _, path := range []string{"/payments/pay_1", "/payments/pay_2", "/healthz", "/nope", "/private"} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	}

	ms := collect(t, reader)
	m, ok := find(ms, "http.server.request.duration")
	if !ok {
		t.Fatal("http.server.request.duration not recorded")
	}
	if m.Unit != "s" {
		t.Errorf("unit = %q, want s", m.Unit)
	}
	routes := map[string]int{}
	for _, dp := range m.Data.(metricdata.Histogram[float64]).DataPoints {
		route, _ := dp.Attributes.Value(routeKey)
		routes[route.AsString()] += int(dp.Count)
		if _, hasPath := dp.Attributes.Value("url.path"); hasPath {
			t.Error("the raw path must not be a metric label")
		}
	}
	want := map[string]int{"/payments/{id}": 2, "": 1, "/private": 1} // a 404 keeps an empty route
	if !maps.Equal(routes, want) {
		t.Errorf("routes = %v, want %v (probes skipped, raw paths collapsed)", routes, want)
	}

	failures, ok := find(ms, "auth.failures")
	if !ok {
		t.Fatal("auth.failures not recorded for the 401")
	}
	dp := failures.Data.(metricdata.Sum[int64]).DataPoints[0]
	if code, _ := dp.Attributes.Value("code"); code.AsString() != "401" || dp.Value != 1 {
		t.Errorf("auth.failures = %v %v, want code=401 value 1", dp.Attributes, dp.Value)
	}

	var names []string
	for _, s := range spans.Ended() {
		names = append(names, s.Name())
	}
	slices.Sort(names)
	if want := []string{"GET", "GET /payments/{id}", "GET /payments/{id}", "GET /private"}; !slices.Equal(names, want) {
		t.Errorf("span names = %v, want %v", names, want)
	}
	if sc := spans.Ended()[0].SpanContext(); !sc.IsValid() || sc.TraceID() == (trace.TraceID{}) {
		t.Error("server span has no valid trace id")
	}
}

func TestTransportInjectsTraceContext(t *testing.T) {
	spans := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	otel.SetTracerProvider(tp)
	if _, err := Setup(context.Background(), Config{}); err != nil { // installs the propagators
		t.Fatal(err)
	}
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = r.Header.Clone() }))
	t.Cleanup(srv.Close)

	ctx, span := tp.Tracer("test").Start(context.Background(), "parent")
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	resp, err := (&http.Client{Transport: Transport(nil)}).Do(req)
	span.End()
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if got.Get("traceparent") == "" || got.Get("X-Amzn-Trace-Id") == "" {
		t.Errorf("outbound request lacks trace headers: %v", got)
	}
	host := strings.TrimPrefix(srv.URL, "http://")
	var client sdktrace.ReadOnlySpan
	for _, s := range spans.Ended() {
		if s.SpanKind() == trace.SpanKindClient {
			client = s
		}
	}
	if client == nil || client.Name() != host {
		t.Errorf("client span name = %v, want the remote host %q (X-Ray names the remote node after it)", client, host)
	}
}

func TestRouteKeyIsTheSemanticConventionName(t *testing.T) {
	if routeKey != attribute.Key("http.route") {
		t.Fatalf("route attribute must be http.route, got %s", routeKey)
	}
}

func TestFromEnv(t *testing.T) {
	env := func(vars map[string]string) *platform.Env {
		return platform.NewEnv(func(k string) (string, bool) { v, ok := vars[k]; return v, ok })
	}
	e := env(nil)
	cfg := FromEnv(e, "demo-api")
	if e.Err() != nil || cfg.Endpoint != "" || cfg.ServiceName != "demo-api" || cfg.SampleRatio != 1 || cfg.MetricInterval != 15*time.Second {
		t.Errorf("defaults = %+v, err %v; want disabled, demo-api, ratio 1, 15s", cfg, e.Err())
	}
	e = env(map[string]string{
		"OTEL_EXPORTER_OTLP_ENDPOINT": "http://otel-collector.observability.svc:4317",
		"OTEL_SERVICE_NAME":           "payments-svc",
		"OTEL_TRACES_SAMPLER_ARG":     "0.1",
		"OTEL_METRIC_EXPORT_INTERVAL": "5000",
	})
	cfg = FromEnv(e, "demo-api")
	if e.Err() != nil || cfg.Endpoint != "http://otel-collector.observability.svc:4317" || cfg.ServiceName != "payments-svc" ||
		cfg.SampleRatio != 0.1 || cfg.MetricInterval != 5*time.Second {
		t.Errorf("from env = %+v, err %v", cfg, e.Err())
	}
	e = env(map[string]string{"OTEL_TRACES_SAMPLER_ARG": "2"})
	if FromEnv(e, "x"); e.Err() == nil {
		t.Error("a sample ratio above 1 must be rejected")
	}
}

func TestSetupWithAnEndpointInstallsSDKProviders(t *testing.T) {
	for _, endpoint := range []string{"127.0.0.1:1", "http://127.0.0.1:1"} {
		t.Run(endpoint, func(t *testing.T) {
			shutdown, err := Setup(context.Background(), Config{
				ServiceName: "demo-api", Endpoint: endpoint, SampleRatio: 1, MetricInterval: time.Hour,
			})
			if err != nil {
				t.Fatalf("Setup: %v (exporters connect lazily, so an unreachable collector is not an error)", err)
			}
			if _, ok := otel.GetTracerProvider().(*sdktrace.TracerProvider); !ok {
				t.Errorf("tracer provider = %T, want the SDK's", otel.GetTracerProvider())
			}
			if _, ok := otel.GetMeterProvider().(*sdkmetric.MeterProvider); !ok {
				t.Errorf("meter provider = %T, want the SDK's", otel.GetMeterProvider())
			}
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			_ = shutdown(ctx) // the collector is unreachable; shutdown must still return within the deadline
		})
	}
}

func TestFlushLogsAFailedShutdown(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	Flush(log, func(context.Context) error { return errors.New("collector unreachable") }, time.Second)
	if !strings.Contains(buf.String(), "telemetry flush failed") {
		t.Errorf("log = %s", buf.String())
	}
	buf.Reset()
	Flush(log, func(ctx context.Context) error {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("Flush must bound the shutdown with a deadline")
		}
		return nil
	}, time.Second)
	if buf.Len() != 0 {
		t.Errorf("a clean flush logs nothing, got %s", buf.String())
	}
}

func TestPublicEndpointDoesNotContinueTheCallersTrace(t *testing.T) {
	spans := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	otel.SetTracerProvider(tp)
	if _, err := Setup(context.Background(), Config{}); err != nil { // installs the propagators
		t.Fatal(err)
	}
	const callerTrace = "0af7651916cd43dd8448eb211c80319c"
	serve := func(opts ...HTTPOption) sdktrace.ReadOnlySpan {
		r := chi.NewRouter()
		r.Use(HTTPMiddleware("demo-api", opts...))
		r.Get("/x", func(http.ResponseWriter, *http.Request) {})
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.Header.Set("traceparent", "00-"+callerTrace+"-b7ad6b7169203331-01")
		r.ServeHTTP(httptest.NewRecorder(), req)
		ended := spans.Ended()
		return ended[len(ended)-1]
	}

	internal := serve()
	if got := internal.SpanContext().TraceID().String(); got != callerTrace {
		t.Errorf("an internal service continues the caller's trace, got %s", got)
	}
	public := serve(AsPublicEndpoint())
	if got := public.SpanContext().TraceID().String(); got == callerTrace {
		t.Error("a public endpoint must start its own trace")
	}
	if links := public.Links(); len(links) != 1 || links[0].SpanContext.TraceID().String() != callerTrace {
		t.Errorf("the caller's context must survive as a link, got %v", links)
	}
}

// installTestProviders makes the global providers record into the returned
// reader and span recorder, and restores the previous ones afterwards.
func installTestProviders(t *testing.T) (*sdkmetric.ManualReader, *tracetest.SpanRecorder) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := newMeterProvider(resource.Empty(), reader)
	spans := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))
	prevMP, prevTP := otel.GetMeterProvider(), otel.GetTracerProvider()
	otel.SetMeterProvider(mp)
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		otel.SetMeterProvider(prevMP)
		otel.SetTracerProvider(prevTP)
		_ = mp.Shutdown(context.Background())
		_ = tp.Shutdown(context.Background())
	})
	return reader, spans
}

// The port a client puts in the Host header must not become a label: any
// caller could otherwise create one series per port.
func TestClientHostPortIsNotAMetricLabel(t *testing.T) {
	reader, _ := installTestProviders(t)
	r := chi.NewRouter()
	r.Use(HTTPMiddleware("demo-api"))
	r.Get("/graphql", func(http.ResponseWriter, *http.Request) {})
	for _, host := range []string{"example.test:1111", "example.test:2222", "example.test"} {
		req := httptest.NewRequest(http.MethodGet, "/graphql", nil)
		req.Host = host
		r.ServeHTTP(httptest.NewRecorder(), req)
	}
	m, ok := find(collect(t, reader), "http.server.request.duration")
	if !ok {
		t.Fatal("http.server.request.duration not recorded")
	}
	points := m.Data.(metricdata.Histogram[float64]).DataPoints
	if len(points) != 1 || points[0].Count != 3 {
		t.Fatalf("3 requests differing only in the Host port must share one series, got %d series", len(points))
	}
	if port, ok := points[0].Attributes.Value("server.port"); ok {
		t.Errorf("server.port must not be a label, got %v", port.String())
	}
}

// A WebSocket lives for minutes: recording its upgrade request as one HTTP
// request would put the whole connection's lifetime into the latency
// histogram. Subscriptions are measured per operation instead.
func TestWebsocketUpgradesAreNotRecordedAsRequests(t *testing.T) {
	reader, spans := installTestProviders(t)
	r := chi.NewRouter()
	r.Use(HTTPMiddleware("demo-api"))
	r.Get("/graphql", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") != "" {
			w.WriteHeader(http.StatusSwitchingProtocols)
		}
	})
	upgrade := httptest.NewRequest(http.MethodGet, "/graphql", nil)
	upgrade.Header.Set("Connection", "Upgrade")
	upgrade.Header.Set("Upgrade", "websocket")
	r.ServeHTTP(httptest.NewRecorder(), upgrade)
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/graphql", nil))

	m, ok := find(collect(t, reader), "http.server.request.duration")
	if !ok {
		t.Fatal("the plain request must still be recorded")
	}
	var total uint64
	for _, dp := range m.Data.(metricdata.Histogram[float64]).DataPoints {
		total += dp.Count
		if code, _ := dp.Attributes.Value("http.response.status_code"); code.AsInt64() == http.StatusSwitchingProtocols {
			t.Error("a WebSocket upgrade must not be recorded as a request")
		}
	}
	if total != 1 {
		t.Errorf("recorded %d requests, want only the plain one", total)
	}
	if n := len(spans.Ended()); n != 1 {
		t.Errorf("server spans = %d, want 1 (none for the upgrade)", n)
	}
}

// Only the subscription endpoint's handshake goes unrecorded: an Upgrade
// header on any other path cannot hide a request from the metrics.
func TestAnUpgradeHeaderElsewhereIsStillRecorded(t *testing.T) {
	reader, _ := installTestProviders(t)
	r := chi.NewRouter()
	r.Use(HTTPMiddleware("demo-api"))
	r.Get("/other", func(http.ResponseWriter, *http.Request) {})
	req := httptest.NewRequest(http.MethodGet, "/other", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	r.ServeHTTP(httptest.NewRecorder(), req)
	m, ok := find(collect(t, reader), "http.server.request.duration")
	if !ok {
		t.Fatal("a GET to another path with an Upgrade header must be recorded")
	}
	var total uint64
	for _, dp := range m.Data.(metricdata.Histogram[float64]).DataPoints {
		total += dp.Count
	}
	if total != 1 {
		t.Fatalf("recorded %d requests, want 1", total)
	}
}
