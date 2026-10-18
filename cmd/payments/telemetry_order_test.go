package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/eliteGoblin/eks-gitops-demo/internal/payments"
	"github.com/eliteGoblin/eks-gitops-demo/internal/payments/memstore"
	"github.com/eliteGoblin/eks-gitops-demo/internal/platform"
)

type panickingAuth struct{}

func (panickingAuth) Authenticate(context.Context, string) (context.Context, error) { panic("boom") }

// Same contract as demo-api: a panic reaches the RED metrics and the trace,
// is answered in the REST envelope, and its log line carries the ids.
func TestPanicIsRecordedAndCorrelated(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	spans := tracetest.NewSpanRecorder()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))
	prevMP, prevTP := otel.GetMeterProvider(), otel.GetTracerProvider()
	otel.SetMeterProvider(mp)
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetMeterProvider(prevMP); otel.SetTracerProvider(prevTP) })

	cfg := config{Issuer: "https://id.example/realms/demo", Audience: "payments-svc", AllowedClients: []string{"demo-api-svc"}, RateRPS: 100, RateBurst: 100}
	cfg.Telemetry.ServiceName = "payments-svc"
	var logs bytes.Buffer
	svc := payments.NewService(memstore.New(), time.Now, func() string { return "pay-1" })
	ready := map[string]platform.Check{"jwks": func(context.Context) error { return nil }}
	h := newRouter(cfg, platform.NewLogger(&logs, 0), panickingAuth{}, ready, svc)

	req := httptest.NewRequest(http.MethodPost, "/payments", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer x")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), `"error"`) {
		t.Fatalf("status = %d body = %s, want a 500 in the REST envelope", rec.Code, rec.Body.String())
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
	if ended := spans.Ended(); len(ended) != 1 || ended[0].Status().Code != codes.Error {
		t.Errorf("want one server span with status Error, got %d spans", len(ended))
	}
	if !strings.Contains(logs.String(), `"trace_id"`) || !strings.Contains(logs.String(), `"request_id"`) {
		t.Errorf("the panic log must carry trace_id and request_id, got %q", logs.String())
	}
}
