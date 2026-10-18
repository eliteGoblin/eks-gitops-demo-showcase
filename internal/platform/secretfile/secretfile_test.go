package secretfile

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// projectedSecret mimics how the kubelet projects a Secret volume: each
// version is a timestamped directory, `..data` points at the current one and
// every key is a symlink through `..data`. An update writes a new directory and
// swaps `..data` with an atomic rename, so a reader never sees a partial file.
type projectedSecret struct {
	t       *testing.T
	dir     string
	version int
}

func newProjectedSecret(t *testing.T, key, value string) (*projectedSecret, string) {
	t.Helper()
	p := &projectedSecret{t: t, dir: t.TempDir()}
	p.write(key, value)
	if err := os.Symlink(filepath.Join("..data", key), filepath.Join(p.dir, key)); err != nil {
		t.Fatal(err)
	}
	return p, filepath.Join(p.dir, key)
}

// write publishes a new version of the Secret the way the kubelet does.
func (p *projectedSecret) write(key, value string) {
	p.t.Helper()
	p.version++
	versionDir := fmt.Sprintf("..2026_10_16_00_00_%02d.%d", p.version, p.version)
	if err := os.Mkdir(filepath.Join(p.dir, versionDir), 0o755); err != nil {
		p.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.dir, versionDir, key), []byte(value), 0o644); err != nil {
		p.t.Fatal(err)
	}
	tmp := filepath.Join(p.dir, "..data_tmp")
	if err := os.Symlink(versionDir, tmp); err != nil {
		p.t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(p.dir, "..data")); err != nil {
		p.t.Fatal(err)
	}
}

func fingerprintOf(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])[:8]
}

type harness struct {
	reader *sdkmetric.ManualReader
	rec    *Recorder
	logs   *bytes.Buffer
	opts   Options
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	rec, err := NewRecorder(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("test"))
	if err != nil {
		t.Fatal(err)
	}
	logs := &bytes.Buffer{}
	return &harness{
		reader: reader, rec: rec, logs: logs,
		opts: Options{Name: "partner-api-key", Interval: time.Hour, Log: slog.New(slog.NewJSONHandler(logs, nil)), Recorder: rec},
	}
}

// reloads returns secret.reloads by source.
func (h *harness) reloads(t *testing.T) map[string]int64 {
	t.Helper()
	got := map[string]int64{}
	for _, m := range h.collect(t) {
		if sum, ok := m.Data.(metricdata.Sum[int64]); ok && m.Name == "secret.reloads" {
			for _, dp := range sum.DataPoints {
				src, _ := dp.Attributes.Value("source")
				got[src.AsString()] += dp.Value
			}
		}
	}
	return got
}

// current returns the fingerprints secret.info reports, as "source=fingerprint".
func (h *harness) current(t *testing.T) []string {
	t.Helper()
	var got []string
	for _, m := range h.collect(t) {
		if g, ok := m.Data.(metricdata.Gauge[int64]); ok && m.Name == "secret.info" {
			for _, dp := range g.DataPoints {
				src, _ := dp.Attributes.Value("source")
				fp, _ := dp.Attributes.Value("fingerprint")
				name, _ := dp.Attributes.Value("name")
				if dp.Value != 1 || name.AsString() != "partner-api-key" {
					t.Fatalf("secret.info point %v = %d, want 1 for partner-api-key", dp.Attributes.ToSlice(), dp.Value)
				}
				got = append(got, src.AsString()+"="+fp.AsString())
			}
		}
	}
	return got
}

func (h *harness) collect(t *testing.T) []metricdata.Metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := h.reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	var out []metricdata.Metrics
	for _, sm := range rm.ScopeMetrics {
		out = append(out, sm.Metrics...)
	}
	return out
}

func TestFingerprintIsAShortSHA256Prefix(t *testing.T) {
	if got, want := Fingerprint("s3cret"), fingerprintOf("s3cret"); got != want || len(got) != 8 {
		t.Fatalf("Fingerprint = %q, want %q (8 hex characters)", got, want)
	}
}

func TestOpenLoadsTheCurrentValue(t *testing.T) {
	h := newHarness(t)
	_, path := newProjectedSecret(t, "api_key", "first-key\n")
	f, err := Open(context.Background(), path, h.opts)
	if err != nil {
		t.Fatal(err)
	}
	if f.Current() != "first-key" {
		t.Fatalf("Current = %q, want the value without the trailing newline", f.Current())
	}
	if f.Fingerprint() != fingerprintOf("first-key") {
		t.Fatalf("Fingerprint = %q", f.Fingerprint())
	}
	if got := h.reloads(t); got["file"] != 1 {
		t.Fatalf("reloads = %v, want file=1 for the first load", got)
	}
	if got := h.current(t); len(got) != 1 || got[0] != "file="+fingerprintOf("first-key") {
		t.Fatalf("secret.info = %v", got)
	}
	if !strings.Contains(h.logs.String(), `"msg":"secret loaded"`) || !strings.Contains(h.logs.String(), fingerprintOf("first-key")) {
		t.Fatalf("the load must be logged with its fingerprint: %s", h.logs)
	}
}

func TestReloadFollowsTheKubeletSymlinkSwap(t *testing.T) {
	h := newHarness(t)
	secret, path := newProjectedSecret(t, "api_key", "first-key")
	f, err := Open(context.Background(), path, h.opts)
	if err != nil {
		t.Fatal(err)
	}
	secret.write("api_key", "second-key")
	f.refresh(context.Background())
	if f.Current() != "second-key" || f.Fingerprint() != fingerprintOf("second-key") {
		t.Fatalf("after the swap Current = %q (%s), want second-key", f.Current(), f.Fingerprint())
	}
	if got := h.reloads(t); got["file"] != 2 {
		t.Fatalf("reloads = %v, want file=2", got)
	}
	// Only the current fingerprint is reported: the old series stops.
	if got := h.current(t); len(got) != 1 || got[0] != "file="+fingerprintOf("second-key") {
		t.Fatalf("secret.info = %v, want only the new fingerprint", got)
	}
	if !strings.Contains(h.logs.String(), `"reload":true`) {
		t.Fatalf("the reload must be logged as one: %s", h.logs)
	}
}

func TestUnchangedContentIsNotAReload(t *testing.T) {
	h := newHarness(t)
	secret, path := newProjectedSecret(t, "api_key", "same-key")
	f, err := Open(context.Background(), path, h.opts)
	if err != nil {
		t.Fatal(err)
	}
	secret.write("api_key", "same-key") // a new version with the same content
	f.refresh(context.Background())
	f.refresh(context.Background())
	if got := h.reloads(t); got["file"] != 1 {
		t.Fatalf("reloads = %v, want only the first load", got)
	}
}

func TestTheValueIsNeverLogged(t *testing.T) {
	h := newHarness(t)
	secret, path := newProjectedSecret(t, "api_key", "first-very-secret-value")
	f, err := Open(context.Background(), path, h.opts)
	if err != nil {
		t.Fatal(err)
	}
	secret.write("api_key", "second-very-secret-value")
	f.refresh(context.Background())
	RecordEnv(context.Background(), "third-very-secret-value", h.opts)
	if strings.Contains(h.logs.String(), "very-secret-value") {
		t.Fatalf("a secret value reached the log: %s", h.logs)
	}
}

func TestOpenFailsClearlyWhenTheFileIsMissingOrEmpty(t *testing.T) {
	h := newHarness(t)
	missing := filepath.Join(t.TempDir(), "api_key")
	if _, err := Open(context.Background(), missing, h.opts); err == nil || !strings.Contains(err.Error(), missing) || !strings.Contains(err.Error(), "partner-api-key") {
		t.Fatalf("a missing file must fail with its name and path: %v", err)
	}
	empty := filepath.Join(t.TempDir(), "api_key")
	if err := os.WriteFile(empty, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), empty, h.opts); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("an empty file must be refused: %v", err)
	}
}

// A failed read after the first load keeps the last good value.
func TestAFailedRefreshKeepsTheLastGoodValue(t *testing.T) {
	h := newHarness(t)
	_, path := newProjectedSecret(t, "api_key", "first-key")
	f, err := Open(context.Background(), path, h.opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	f.refresh(context.Background())
	if f.Current() != "first-key" {
		t.Fatalf("Current = %q, want the last good value", f.Current())
	}
	if !strings.Contains(h.logs.String(), `"msg":"secret reload failed"`) {
		t.Fatalf("the failed read must be logged: %s", h.logs)
	}
}

func TestRunPicksUpARotation(t *testing.T) {
	h := newHarness(t)
	h.opts.Interval = 5 * time.Millisecond
	secret, path := newProjectedSecret(t, "api_key", "first-key")
	f, err := Open(context.Background(), path, h.opts)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { f.Run(ctx); close(done) }()
	secret.write("api_key", "second-key")
	deadline := time.Now().Add(2 * time.Second)
	for f.Current() != "second-key" {
		if time.Now().After(deadline) {
			t.Fatalf("Run did not pick up the rotation; Current = %q", f.Current())
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not stop with its context")
	}
}

func TestCurrentIsSafeDuringReloads(t *testing.T) {
	h := newHarness(t)
	secret, path := newProjectedSecret(t, "api_key", "key-0")
	f, err := Open(context.Background(), path, h.opts)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					if v := f.Current(); !strings.HasPrefix(v, "key-") {
						t.Errorf("Current returned a partial value %q", v)
						return
					}
				}
			}
		}()
	}
	for i := 1; i <= 20; i++ {
		secret.write("api_key", fmt.Sprintf("key-%d", i))
		f.refresh(context.Background())
	}
	close(stop)
	wg.Wait()
	if f.Current() != "key-20" {
		t.Fatalf("Current = %q, want key-20", f.Current())
	}
}

func TestRecordEnvReportsTheEnvironmentDelivery(t *testing.T) {
	h := newHarness(t)
	RecordEnv(context.Background(), "env-key", h.opts)
	_, path := newProjectedSecret(t, "api_key", "file-key")
	if _, err := Open(context.Background(), path, h.opts); err != nil {
		t.Fatal(err)
	}
	if got := h.reloads(t); got["env"] != 1 || got["file"] != 1 {
		t.Fatalf("reloads = %v, want env=1 and file=1", got)
	}
	got := h.current(t)
	want := map[string]bool{"env=" + fingerprintOf("env-key"): true, "file=" + fingerprintOf("file-key"): true}
	if len(got) != 2 || !want[got[0]] || !want[got[1]] {
		t.Fatalf("secret.info = %v, want both deliveries", got)
	}
}

// A nil Recorder and a nil logger are valid: the kit then only delivers.
func TestNoTelemetryIsFine(t *testing.T) {
	_, path := newProjectedSecret(t, "api_key", "key")
	f, err := Open(context.Background(), path, Options{Name: "k"})
	if err != nil || f.Current() != "key" {
		t.Fatalf("Open without telemetry: %q, %v", f.Current(), err)
	}
	RecordEnv(context.Background(), "key", Options{Name: "k"})
}

// Attribute keys are part of the dashboard contract (secret_info{name,source,fingerprint}).
func TestInfoAttributes(t *testing.T) {
	got := infoAttributes(key{name: "n", source: "file"}, "abcd1234")
	want := []attribute.KeyValue{attribute.String("name", "n"), attribute.String("source", "file"), attribute.String("fingerprint", "abcd1234")}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("attributes = %v, want %v", got, want)
	}
}
