package platform

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// scriptedCheck answers with the next scripted result on every call.
type scriptedCheck struct {
	results []error
	calls   atomic.Int64
}

func (s *scriptedCheck) check(context.Context) error {
	n := int(s.calls.Add(1)) - 1
	if n < len(s.results) {
		return s.results[n]
	}
	return nil
}

func newTestCachedCheck(results ...error) (*CachedCheck, *time.Time) {
	now := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
	s := &scriptedCheck{results: results}
	c := NewCachedCheck(s.check, CachedCheckOptions{Interval: 5 * time.Second, Timeout: time.Second, Threshold: 3})
	c.now = func() time.Time { return now }
	return c, &now
}

var errDown = errors.New("connection refused")

// The probe answers from memory: not ready until a check has passed, ready
// through short blips, not ready after Threshold failures in a row, ready
// again on the next success.
func TestCachedCheckToleratesBlipsButNotOutages(t *testing.T) {
	ctx := context.Background()
	c, _ := newTestCachedCheck(nil, errDown, errDown, errDown, nil)
	if err := c.Check(ctx); err == nil {
		t.Fatal("not ready before the first check")
	}
	c.probe(ctx) // ok
	if err := c.Check(ctx); err != nil {
		t.Fatalf("ready after a passing check: %v", err)
	}
	c.probe(ctx) // fail 1
	c.probe(ctx) // fail 2
	if err := c.Check(ctx); err != nil {
		t.Fatalf("two failures in a row are a blip: %v", err)
	}
	c.probe(ctx) // fail 3
	if err := c.Check(ctx); err == nil || !errors.Is(err, errDown) || !strings.Contains(err.Error(), "3 checks in a row") {
		t.Fatalf("three failures in a row are an outage, got %v", err)
	}
	c.probe(ctx) // ok again
	if err := c.Check(ctx); err != nil {
		t.Fatalf("ready again after a passing check: %v", err)
	}
}

// A checker that stopped running (stuck or ended) cannot keep the pod ready.
func TestCachedCheckGoesStale(t *testing.T) {
	ctx := context.Background()
	c, now := newTestCachedCheck()
	c.probe(ctx)
	*now = now.Add(c.stale - time.Millisecond)
	if err := c.Check(ctx); err != nil {
		t.Fatalf("still fresh: %v", err)
	}
	*now = now.Add(2 * time.Millisecond)
	if err := c.Check(ctx); err == nil {
		t.Fatal("no recent check: not ready")
	}
}

// Run checks at once and then on every interval until its context ends.
func TestCachedCheckRunsUntilCancelled(t *testing.T) {
	s := &scriptedCheck{}
	c := NewCachedCheck(s.check, CachedCheckOptions{Interval: 5 * time.Millisecond, Timeout: time.Second, Threshold: 3})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	deadline := time.After(2 * time.Second)
	for s.calls.Load() < 3 {
		select {
		case <-deadline:
			t.Fatalf("only %d checks ran", s.calls.Load())
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not stop")
	}
	if err := c.Check(context.Background()); err != nil {
		t.Fatalf("passing checks: %v", err)
	}
}

// Each check gets its own timeout, so a hung dependency counts as a failure.
func TestCachedCheckTimesOutAHungCheck(t *testing.T) {
	hung := func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }
	c := NewCachedCheck(hung, CachedCheckOptions{Interval: time.Second, Timeout: 10 * time.Millisecond, Threshold: 1})
	c.probe(context.Background())
	if err := c.Check(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want a timed-out check, got %v", err)
	}
}
