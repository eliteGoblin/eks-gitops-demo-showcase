package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"
)

func TestEnv(t *testing.T) {
	vars := map[string]string{"NAME": " api ", "URL": "https://id.example/realms/demo", "BAD_URL": "not a url", "MODE": "prod", "LIST": "a, ,b", "B": "true", "F": "2.5", "BADB": "maybe", "BADF": "-1", "I": "40", "BADI": "0.5"}
	env := NewEnv(func(k string) (string, bool) { v, ok := vars[k]; return v, ok })

	if env.String("NAME", "x") != "api" || env.String("MISSING", "def") != "def" {
		t.Error("String")
	}
	if env.URL("URL", "") != vars["URL"] {
		t.Error("URL")
	}
	if got := env.List("LIST"); len(got) != 2 || got[1] != "b" {
		t.Errorf("List = %v", got)
	}
	if env.OneOf("MODE", "dev", "dev", "prod") != "prod" || env.Err() != nil {
		t.Fatalf("valid config reported errors: %v", env.Err())
	}
	if !env.Bool("B", false) || env.Bool("NOB", false) || env.Float("F", 1) != 2.5 || env.Float("NOF", 3) != 3 || env.Err() != nil {
		t.Fatalf("Bool/Float: %v", env.Err())
	}
	if env.Int("I", 1) != 40 || env.Int("NOI", 7) != 7 || env.Err() != nil {
		t.Fatalf("Int: %v", env.Err())
	}
	env.Bool("BADB", false)
	env.Float("BADF", 1)
	env.Int("BADI", 1)
	env.Errorf("custom %s", "problem")
	env.Required("MISSING")
	env.URL("BAD_URL", "")
	env.OneOf("NAME", "", "dev")
	err := env.Err()
	for _, want := range []string{"MISSING is required", "BAD_URL must be", "NAME must be one of", "BADB must be a boolean", "BADF must be a positive number", "BADI must be a positive integer", "custom problem"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Err() = %v, want it to mention %q", err, want)
		}
	}
	if NewEnv(nil).lookup == nil {
		t.Error("nil lookup must default to os.LookupEnv")
	}
}

func TestReadyz(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	ok := Readyz(time.Second, map[string]Check{"jwks": func(context.Context) error { return nil }}, quiet)
	rec := httptest.NewRecorder()
	ok(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != 200 {
		t.Fatalf("ready: %d", rec.Code)
	}
	bad := Readyz(time.Second, map[string]Check{"db": func(context.Context) error { return errors.New("dial tcp 10.0.0.5:5432: refused") }}, quiet)
	rec = httptest.NewRecorder()
	bad(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	// The probe is public: name the failed check, keep the details in the logs.
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), `"db"`) || strings.Contains(rec.Body.String(), "10.0.0.5") {
		t.Fatalf("not ready: %d %s", rec.Code, rec.Body)
	}
	rec = httptest.NewRecorder()
	Healthz(rec, nil)
	if rec.Body.String() != `{"status":"ok"}` {
		t.Fatal("healthz body")
	}
}

func TestDraining(t *testing.T) {
	var d Draining
	if err := d.Check(context.Background()); err != nil {
		t.Fatalf("not draining yet: %v", err)
	}
	d.Begin()
	if err := d.Check(context.Background()); err == nil {
		t.Fatal("readiness must fail once draining begins")
	}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func waitUp(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		resp, err := http.Get("http://" + addr) //nolint:noctx // test helper
		if err == nil {
			_ = resp.Body.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("server never came up: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// On SIGTERM the server first reports not-ready while still serving (so the
// load balancer stops sending traffic), then drains and stops.
func TestServeShutsDownGracefully(t *testing.T) {
	addr := freeAddr(t)
	var draining Draining
	srv := &http.Server{Addr: addr, Handler: http.HandlerFunc(Healthz), ReadHeaderTimeout: time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, srv, Shutdown{Delay: 300 * time.Millisecond, Timeout: time.Second, OnStart: draining.Begin})
	}()
	waitUp(t, addr)

	cancel()
	time.Sleep(100 * time.Millisecond) // inside the delay window
	if draining.Check(context.Background()) == nil {
		t.Fatal("readiness must fail as soon as shutdown starts")
	}
	resp, err := http.Get("http://" + addr) //nolint:noctx // test helper
	if err != nil {
		t.Fatalf("requests during the delay must still be served: %v", err)
	}
	_ = resp.Body.Close()
	if err := <-done; err != nil {
		t.Fatalf("Serve returned %v after cancel", err)
	}

	bad := &http.Server{Addr: "256.0.0.1:1", ReadHeaderTimeout: time.Second}
	if err := Serve(context.Background(), bad, Shutdown{Timeout: time.Second}); err == nil {
		t.Fatal("listen error must be returned")
	}
}

// A drain that runs out of time closes the remaining connections and reports it.
func TestServeForceClosesAfterTimeout(t *testing.T) {
	addr := freeAddr(t)
	release := make(chan struct{})
	slow := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			<-release
		}
	})
	srv := &http.Server{Addr: addr, Handler: slow, ReadHeaderTimeout: time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, srv, Shutdown{Timeout: 100 * time.Millisecond}) }()
	waitUp(t, addr)
	go func() {
		resp, err := http.Get("http://" + addr + "/slow") //nolint:noctx // test helper
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	err := <-done
	close(release)
	if err == nil || !strings.Contains(err.Error(), "graceful shutdown") {
		t.Fatalf("want a graceful-shutdown timeout error, got %v", err)
	}
}

func TestWriteErrorWithoutRequestID(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteError(rec, httptest.NewRequest(http.MethodGet, "/", nil), http.StatusForbidden, CodeForbidden, "no")
	if rec.Code != 403 || strings.Contains(rec.Body.String(), "request_id") {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
}

func TestWriteErrorRESTFormat(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r = r.WithContext(WithErrorFormat(WithRequestID(r.Context(), "req-1"), RESTErrors))
	rec := httptest.NewRecorder()
	WriteError(rec, r, http.StatusNotFound, CodeNotFound, "payment not found")
	var env map[string]map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("body %s: %v", rec.Body, err)
	}
	got := env["error"]
	if rec.Code != http.StatusNotFound || got["code"] != CodeNotFound || got["message"] != "payment not found" || got["request_id"] != "req-1" {
		t.Fatalf("status %d body %s", rec.Code, rec.Body)
	}
}

func TestLoggerAddsTraceIDs(t *testing.T) {
	var buf bytes.Buffer
	log := NewLogger(&buf, slog.LevelInfo)
	tid, _ := trace.TraceIDFromHex("5759e988bd862e3fe1be46a994272793")
	sid, _ := trace.SpanIDFromHex("53995c3f42cd8ad8")
	ctx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid, TraceFlags: trace.FlagsSampled}))

	log.InfoContext(ctx, "traced")
	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"trace_id":      "5759e988bd862e3fe1be46a994272793",
		"span_id":       "53995c3f42cd8ad8",
		"xray_trace_id": "1-5759e988-bd862e3fe1be46a994272793", // searchable as-is in X-Ray
	}
	for k, v := range want {
		if rec[k] != v {
			t.Errorf("%s = %v, want %s", k, rec[k], v)
		}
	}

	buf.Reset()
	log.InfoContext(context.Background(), "untraced")
	if strings.Contains(buf.String(), "trace_id") {
		t.Errorf("a record without a span must not carry trace ids: %s", buf.String())
	}
}
