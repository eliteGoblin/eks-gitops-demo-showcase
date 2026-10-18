package secretfile

import (
	"context"
	"fmt"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Recorder reports secret loads as metrics (Prometheus names after the
// collector's remote write):
//
//	secret_reloads_total{name,source}         loads, including the first
//	secret_info{name,source,fingerprint} = 1  the fingerprint each secret has now
//
// secret_info reports only the current fingerprint, so a rotation ends the old
// series: a dashboard table shows, per pod, which version it holds.
type Recorder struct {
	reloads metric.Int64Counter

	mu      sync.Mutex
	current map[key]string // fingerprint by secret and delivery
}

type key struct{ name, source string }

// NewRecorder registers the instruments on meter.
func NewRecorder(meter metric.Meter) (*Recorder, error) {
	reloads, err := meter.Int64Counter("secret.reloads",
		metric.WithUnit("{load}"),
		metric.WithDescription("Secret loads, including the first, by secret and delivery (file or env)"))
	if err != nil {
		return nil, fmt.Errorf("secret.reloads: %w", err)
	}
	info, err := meter.Int64ObservableGauge("secret.info",
		metric.WithUnit("{secret}"),
		metric.WithDescription("1 for the fingerprint each secret currently has, by delivery"))
	if err != nil {
		return nil, fmt.Errorf("secret.info: %w", err)
	}
	r := &Recorder{reloads: reloads, current: map[key]string{}}
	if _, err := meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		for k, fingerprint := range r.snapshot() {
			o.ObserveInt64(info, 1, metric.WithAttributes(infoAttributes(k, fingerprint)...))
		}
		return nil
	}, info); err != nil {
		return nil, fmt.Errorf("secret.info callback: %w", err)
	}
	return r, nil
}

func (r *Recorder) loaded(ctx context.Context, k key, fingerprint string) {
	r.mu.Lock()
	r.current[k] = fingerprint
	r.mu.Unlock()
	r.reloads.Add(ctx, 1, metric.WithAttributes(attribute.String("name", k.name), attribute.String("source", k.source)))
}

func (r *Recorder) snapshot() map[key]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[key]string, len(r.current))
	for k, v := range r.current {
		out[k] = v
	}
	return out
}

func infoAttributes(k key, fingerprint string) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String("name", k.name),
		attribute.String("source", k.source),
		attribute.String("fingerprint", fingerprint),
	}
}
