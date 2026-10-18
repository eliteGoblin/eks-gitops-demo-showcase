package platform

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// CachedCheckOptions tune a CachedCheck; zero values take the defaults.
type CachedCheckOptions struct {
	Interval  time.Duration // between checks; default 5s
	Timeout   time.Duration // per check; default 2s
	Threshold int           // consecutive failures before not-ready; default 3
	Name      string        // for the log
	Log       *slog.Logger  // failures are logged at WARN; nil logs nothing
}

// CachedCheck runs a dependency check in the background and answers the
// readiness probe from its recent results, so the probe never queues behind
// request traffic for a shared resource (such as a busy connection pool) and a
// short blip does not take every replica out of the load balancer at once.
// It reports not ready until the first check passes, after Threshold failures
// in a row, and when no check has finished recently (the checker stopped).
type CachedCheck struct {
	check     Check
	interval  time.Duration
	timeout   time.Duration
	threshold int
	stale     time.Duration
	name      string
	log       *slog.Logger
	now       func() time.Time

	mu       sync.Mutex
	passed   bool      // a check has passed at least once
	failures int       // consecutive failures
	lastErr  error     // error of the latest failure
	lastRun  time.Time // when the latest check finished
}

var (
	errNotChecked = errors.New("not checked yet")
	errStale      = errors.New("no recent check")
)

// NewCachedCheck wraps check; call Run to start checking.
func NewCachedCheck(check Check, opts CachedCheckOptions) *CachedCheck {
	c := &CachedCheck{
		check: check, interval: opts.Interval, timeout: opts.Timeout, threshold: opts.Threshold,
		name: opts.Name, log: opts.Log, now: time.Now,
	}
	if c.interval <= 0 {
		c.interval = 5 * time.Second
	}
	if c.timeout <= 0 {
		c.timeout = 2 * time.Second
	}
	if c.threshold <= 0 {
		c.threshold = 3
	}
	c.stale = 3*c.interval + c.timeout
	return c
}

// Run checks at once and then every interval until ctx ends.
func (c *CachedCheck) Run(ctx context.Context) {
	c.probe(ctx)
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.probe(ctx)
		}
	}
}

// probe runs the check once, bounded by the timeout, and records the outcome.
func (c *CachedCheck) probe(ctx context.Context) {
	checkCtx, cancel := context.WithTimeout(ctx, c.timeout)
	err := c.check(checkCtx)
	cancel()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastRun = c.now()
	if err == nil {
		c.passed, c.failures, c.lastErr = true, 0, nil
		return
	}
	c.failures++
	c.lastErr = err
	if c.log != nil {
		c.log.WarnContext(ctx, "dependency check failed", "check", c.name, "failures", c.failures, "err", err)
	}
}

// Check is the readiness check: it reads the recorded outcome and never blocks.
func (c *CachedCheck) Check(context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case !c.passed && c.failures == 0:
		return errNotChecked
	case c.now().Sub(c.lastRun) > c.stale:
		return errStale
	case !c.passed || c.failures >= c.threshold:
		return fmt.Errorf("failed %d checks in a row: %w", c.failures, c.lastErr)
	}
	return nil
}
