// Package otel is the kit's OpenTelemetry setup: one call wires traces and
// metrics to the in-cluster collector (OTLP/gRPC), with X-Ray-compatible
// trace ids and propagation, and gives every duration histogram seconds-based
// buckets. With no endpoint configured the providers stay no-ops, so local
// runs need no collector.
//
// Subpackages instrument the frameworks the core must not depend on:
// otelgql (gqlgen) and otelpgx (pgx).
package otel

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/contrib/propagators/aws/xray"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/eliteGoblin/eks-gitops-demo/internal/platform"
)

// ScopeName is the instrumentation scope of every instrument the kit creates.
const ScopeName = "github.com/eliteGoblin/eks-gitops-demo/internal/platform/otel"

// Config selects where telemetry goes and how much of it.
type Config struct {
	// ServiceName becomes service.name: the X-Ray service node and the
	// Prometheus job label. Required when Endpoint is set.
	ServiceName string
	// Endpoint is the collector's OTLP/gRPC address (host:port, or an
	// http(s):// URL). Empty disables exporting.
	Endpoint string
	// SampleRatio is the share of new traces kept, in (0, 1]. A sampled parent
	// is always followed.
	SampleRatio float64
	// MetricInterval is how often metrics are pushed to the collector.
	MetricInterval time.Duration
}

const (
	defaultMetricIntervalMs = 15000
	exportTimeout           = 10 * time.Second
)

// FromEnv reads the standard variables the kit supports:
// OTEL_EXPORTER_OTLP_ENDPOINT, OTEL_SERVICE_NAME (default service),
// OTEL_TRACES_SAMPLER_ARG (default 1.0; the sampler is always parent-based
// trace-id ratio) and OTEL_METRIC_EXPORT_INTERVAL (milliseconds).
func FromEnv(env *platform.Env, service string) Config {
	cfg := Config{
		ServiceName:    env.String("OTEL_SERVICE_NAME", service),
		Endpoint:       env.String("OTEL_EXPORTER_OTLP_ENDPOINT", ""),
		SampleRatio:    env.Float("OTEL_TRACES_SAMPLER_ARG", 1.0),
		MetricInterval: time.Duration(env.Int("OTEL_METRIC_EXPORT_INTERVAL", defaultMetricIntervalMs)) * time.Millisecond,
	}
	if cfg.SampleRatio > 1 {
		env.Errorf("OTEL_TRACES_SAMPLER_ARG must be at most 1, got %v", cfg.SampleRatio)
	}
	return cfg
}

// DurationBuckets are the seconds-based histogram boundaries every duration
// instrument uses. The SDK's defaults are sized for milliseconds and would put
// almost every request in the first bucket.
var DurationBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

// Setup installs the propagators (W3C trace context, baggage and X-Ray) and,
// when an endpoint is set, the global tracer and meter providers plus Go
// runtime metrics. The returned shutdown flushes and stops the providers; it
// is safe to call when exporting is disabled.
func Setup(ctx context.Context, cfg Config) (shutdown func(context.Context) error, err error) {
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{}, xray.Propagator{},
	))
	if cfg.Endpoint == "" {
		return func(context.Context) error { return nil }, nil
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	// Only service.name: with resource_to_telemetry_conversion on the collector
	// every resource attribute becomes a label on every series, and
	// service.namespace would turn the Prometheus job label into
	// "<namespace>/<service>".
	res := resource.NewSchemaless(attribute.String("service.name", cfg.ServiceName))
	traceExp, err := otlptracegrpc.New(ctx, traceEndpoint(cfg.Endpoint)...)
	if err != nil {
		return nil, fmt.Errorf("otel: trace exporter: %w", err)
	}
	metricExp, err := otlpmetricgrpc.New(ctx, metricEndpoint(cfg.Endpoint)...)
	if err != nil {
		return nil, fmt.Errorf("otel: metric exporter: %w", err)
	}
	tp := newTracerProvider(res, traceExp, cfg.SampleRatio)
	mp := newMeterProvider(res, sdkmetric.NewPeriodicReader(metricExp,
		sdkmetric.WithInterval(cfg.MetricInterval), sdkmetric.WithTimeout(exportTimeout)))
	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)
	if err := runtime.Start(runtime.WithMeterProvider(mp)); err != nil {
		return nil, fmt.Errorf("otel: runtime metrics: %w", err)
	}
	return func(ctx context.Context) error {
		return errors.Join(tp.Shutdown(ctx), mp.Shutdown(ctx))
	}, nil
}

// Flush runs the shutdown Setup returned within timeout and logs a failure.
// Defer it in main: it runs after the server has drained, so the last
// requests' spans and metrics are exported before the process exits.
func Flush(log *slog.Logger, shutdown func(context.Context) error, timeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := shutdown(ctx); err != nil {
		log.Warn("telemetry flush failed", "err", err)
	}
}

func (c Config) validate() error {
	if c.ServiceName == "" {
		return errors.New("otel: ServiceName is required when an endpoint is set")
	}
	if c.SampleRatio <= 0 || c.SampleRatio > 1 {
		return fmt.Errorf("otel: SampleRatio must be in (0, 1], got %v", c.SampleRatio)
	}
	if c.MetricInterval <= 0 {
		return fmt.Errorf("otel: MetricInterval must be positive, got %v", c.MetricInterval)
	}
	return nil
}

func newTracerProvider(res *resource.Resource, exp sdktrace.SpanExporter, ratio float64) *sdktrace.TracerProvider {
	root := sdktrace.TraceIDRatioBased(ratio)
	return sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithBatcher(exp, sdktrace.WithExportTimeout(exportTimeout)),
		// X-Ray rejects trace ids whose first 4 bytes are not a recent epoch.
		sdktrace.WithIDGenerator(xray.NewIDGenerator()),
		sdktrace.WithSampler(sdktrace.ParentBased(root)),
	)
}

func newMeterProvider(res *resource.Resource, reader sdkmetric.Reader) *sdkmetric.MeterProvider {
	return sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(reader),
		sdkmetric.WithView(secondsHistogramView()),
	)
}

// secondsHistogramView gives every histogram recorded in seconds the
// DurationBuckets boundaries, whichever instrumentation created it.
func secondsHistogramView() sdkmetric.View {
	return sdkmetric.NewView(
		sdkmetric.Instrument{Kind: sdkmetric.InstrumentKindHistogram, Unit: "s"},
		sdkmetric.Stream{Aggregation: sdkmetric.AggregationExplicitBucketHistogram{Boundaries: DurationBuckets}},
	)
}

func traceEndpoint(endpoint string) []otlptracegrpc.Option {
	if strings.Contains(endpoint, "://") {
		return []otlptracegrpc.Option{otlptracegrpc.WithEndpointURL(endpoint)}
	}
	return []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(endpoint), otlptracegrpc.WithInsecure()}
}

func metricEndpoint(endpoint string) []otlpmetricgrpc.Option {
	if strings.Contains(endpoint, "://") {
		return []otlpmetricgrpc.Option{otlpmetricgrpc.WithEndpointURL(endpoint)}
	}
	return []otlpmetricgrpc.Option{otlpmetricgrpc.WithEndpoint(endpoint), otlpmetricgrpc.WithInsecure()}
}

// Meter returns the kit's meter from the current global provider. Create
// instruments when the component is built (after Setup), not in package
// variables.
func Meter() metric.Meter { return otel.GetMeterProvider().Meter(ScopeName) }

// AuthFailures is the auth.failures counter: requests refused as 401 (not
// authenticated) or 403 (not allowed), labelled by code. The HTTP middleware
// and the GraphQL extension both count into it.
func AuthFailures(m metric.Meter) metric.Int64Counter {
	c, err := m.Int64Counter("auth.failures",
		metric.WithUnit("{failure}"),
		metric.WithDescription("Requests refused because the caller was not authenticated (401) or not allowed (403)."))
	if err != nil {
		otel.Handle(err)
	}
	return c
}

// AuthFailureCode is the code attribute of auth.failures.
func AuthFailureCode(status int) attribute.KeyValue {
	return attribute.String("code", fmt.Sprint(status))
}
