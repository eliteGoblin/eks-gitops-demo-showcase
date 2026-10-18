// Package secretfile delivers a secret to the process and lets it rotate
// without a restart.
//
// Kubernetes projects a Secret volume as files behind a symlink (`..data`)
// that the kubelet swaps atomically when the Secret changes. File polls its
// path and swaps the in-memory value when the content changes, so callers
// always read a complete value with Current() and pick up a rotation within
// one poll interval of the kubelet's update. Mount the whole volume: a subPath
// mount is never updated.
//
// The value is never logged. Every load is reported by a fingerprint, the
// first 8 hex characters of its SHA-256: enough to tell versions apart in logs
// and dashboards, useless for recovering a high-entropy key. (It is not
// suitable for low-entropy secrets such as passwords, where a fingerprint
// could confirm a guess.)
package secretfile

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"
)

// How a secret reached the process.
const (
	SourceFile = "file" // a mounted file, re-read on change
	SourceEnv  = "env"  // an environment variable, fixed until the next restart
)

const defaultInterval = 10 * time.Second

// Options configure loading and reporting; only Name is required.
type Options struct {
	Name     string        // the secret's name in logs and metrics, e.g. "partner-api-key"
	Interval time.Duration // how often File re-reads its path; default 10s
	Log      *slog.Logger  // loads at INFO, failed reloads at WARN; nil logs nothing
	Recorder *Recorder     // nil records no metrics
}

// File is a secret read from a file that may be replaced at any time.
type File struct {
	path string
	opts Options

	mu          sync.RWMutex
	value       string
	fingerprint string
}

// Fingerprint identifies a version of a secret without revealing it.
func Fingerprint(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:8]
}

// Open reads the secret at path. A missing or empty file is an error, so a
// broken mount fails startup instead of running with no secret.
func Open(ctx context.Context, path string, opts Options) (*File, error) {
	if opts.Interval <= 0 {
		opts.Interval = defaultInterval
	}
	f := &File{path: path, opts: opts}
	value, err := f.read()
	if err != nil {
		return nil, err
	}
	f.swap(ctx, value, false)
	return f, nil
}

// Current returns the latest value. Callers should read it for every use (for
// example per outbound request) rather than caching it, so a rotation takes
// effect without a restart.
func (f *File) Current() string {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.value
}

// Fingerprint returns the fingerprint of the latest value.
func (f *File) Fingerprint() string {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.fingerprint
}

// Run re-reads the file every Interval until ctx is done.
func (f *File) Run(ctx context.Context) {
	ticker := time.NewTicker(f.opts.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			f.refresh(ctx)
		}
	}
}

// refresh swaps in the file's content when it changed. A failed read keeps
// the last good value: a rotation glitch must not take the secret away.
func (f *File) refresh(ctx context.Context) {
	value, err := f.read()
	if err != nil {
		if f.opts.Log != nil {
			f.opts.Log.WarnContext(ctx, "secret reload failed", "name", f.opts.Name, "err", err)
		}
		return
	}
	if value == f.Current() {
		return
	}
	f.swap(ctx, value, true)
}

// read returns the file's content without its trailing line break. The
// kubelet swaps the whole directory, so the content is always complete.
func (f *File) read() (string, error) {
	raw, err := os.ReadFile(f.path)
	if err != nil {
		return "", fmt.Errorf("secret %s: %w", f.opts.Name, err)
	}
	value := string(bytes.TrimRight(raw, "\r\n"))
	if value == "" {
		return "", fmt.Errorf("secret %s: %s is empty", f.opts.Name, f.path)
	}
	return value, nil
}

func (f *File) swap(ctx context.Context, value string, reload bool) {
	fingerprint := Fingerprint(value)
	f.mu.Lock()
	f.value, f.fingerprint = value, fingerprint
	f.mu.Unlock()
	report(ctx, f.opts, SourceFile, fingerprint, reload)
}

// RecordEnv reports a secret delivered as an environment variable. Its value
// is fixed for the life of the process: a rotation needs a restart (Reloader
// rolls the pods when the Secret behind the variable changes).
func RecordEnv(ctx context.Context, value string, opts Options) {
	report(ctx, opts, SourceEnv, Fingerprint(value), false)
}

func report(ctx context.Context, opts Options, source, fingerprint string, reload bool) {
	if opts.Log != nil {
		opts.Log.InfoContext(ctx, "secret loaded", "name", opts.Name, "source", source, "fingerprint", fingerprint, "reload", reload)
	}
	if opts.Recorder != nil {
		opts.Recorder.loaded(ctx, key{name: opts.Name, source: source}, fingerprint)
	}
}
