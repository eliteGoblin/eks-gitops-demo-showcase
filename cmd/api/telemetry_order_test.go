package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/eliteGoblin/eks-gitops-demo/internal/platform"
)

// panickingAuth panics inside the chain, like any handler bug would.
type panickingAuth struct{}

func (panickingAuth) Authenticate(context.Context, string) (context.Context, error) { panic("boom") }

// A panic is a server error like any other: it must reach the RED metrics and
// the trace, and its log line must carry the ids that tie it to them. So
// recovery sits inside tracing, after the request id.
func TestPanicIsRecordedAndCorrelated(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	spans := tracetest.NewSpanRecorder()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))
	prevMP, prevTP := otel.GetMeterProvider(), otel.GetTracerProvider()
	otel.SetMeterProvider(mp)
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetMeterProvider(prevMP); otel.SetTracerProvider(prevTP) })

	cfg, err := loadConfig(envOf(map[string]string{"OIDC_ISSUER": "https://id.example/realms/demo"}))
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	ready := map[string]platform.Check{"jwks": func(context.Context) error { return nil }}
	h := newRouter(cfg, platform.NewLogger(&logs, 0), panickingAuth{}, ready, http.NotFoundHandler())

	req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(`{"query":"{__typename}"}`))
	req.Header.Set("Authorization", "Bearer x")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	recorded500 := false
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "http.server.request.duration" {
				continue
			}
			for _, dp := range m.Data.(metricdata.Histogram[float64]).DataPoints {
				if code, _ := dp.Attributes.Value("http.response.status_code"); code.AsInt64() == 500 {
					recorded500 = true
				}
			}
		}
	}
	if !recorded500 {
		t.Error("the panic's 500 must be recorded in http.server.request.duration")
	}
	ended := spans.Ended()
	if len(ended) != 1 || ended[0].Status().Code != codes.Error {
		t.Errorf("want one server span with status Error, got %d spans", len(ended))
	}
	var panicLine string
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, `"msg":"panic"`) {
			panicLine = line
		}
	}
	for _, key := range []string{`"trace_id"`, `"request_id"`} {
		if !strings.Contains(panicLine, key) {
			t.Errorf("the panic log must carry %s, got %q", key, panicLine)
		}
	}
}
