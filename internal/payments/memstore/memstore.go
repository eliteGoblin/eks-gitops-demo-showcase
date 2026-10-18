// Package memstore is the in-memory payments.Store. Data lives in the
// process, so it suits a single replica demo; a database replaces it later.
// Payments expire after a retention window and the store has a hard size cap,
// so a long-running replica cannot grow without bound.
package memstore

import (
	"context"
	"sync"
	"time"

	"github.com/eliteGoblin/eks-gitops-demo/internal/payments"
)

// Defaults: idempotency keys are honoured for a day (like most payment APIs),
// and a replica holds at most this many payments before refusing new ones.
const (
	DefaultTTL         = 24 * time.Hour
	DefaultMaxPayments = 10_000
)

// Options tune retention; zero values take the defaults.
type Options struct {
	TTL         time.Duration    // how long a payment (and its key) is remembered
	MaxPayments int              // hard cap; CreateOnce returns payments.ErrStoreFull beyond it
	Now         func() time.Time // the clock; tests pass a fixed one
}

// Store keeps payments by id and by idempotency key.
type Store struct {
	mu        sync.Mutex
	byID      map[string]payments.Payment
	byKey     map[string]string // idempotency key → payment id
	ttl       time.Duration
	max       int
	now       func() time.Time
	lastSweep time.Time
}

// New returns an empty store with the default retention.
func New() *Store { return NewWithOptions(Options{}) }

// NewWithOptions returns an empty store with the given retention.
func NewWithOptions(o Options) *Store {
	if o.TTL <= 0 {
		o.TTL = DefaultTTL
	}
	if o.MaxPayments <= 0 {
		o.MaxPayments = DefaultMaxPayments
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Store{byID: map[string]payments.Payment{}, byKey: map[string]string{}, ttl: o.TTL, max: o.MaxPayments, now: o.Now}
}

// CreateOnce stores p under key, or returns the payment the key already holds.
// It fails with payments.ErrStoreFull when the cap is reached.
func (s *Store) CreateOnce(_ context.Context, key string, p payments.Payment) (payments.Payment, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.sweep(now)
	if id, ok := s.byKey[key]; ok {
		return s.byID[id], false, nil
	}
	if len(s.byID) >= s.max {
		return payments.Payment{}, false, payments.ErrStoreFull
	}
	s.byKey[key] = p.ID
	s.byID[p.ID] = p
	return p, true, nil
}

// Get returns the payment with id, or payments.ErrNotFound (also once it has expired).
func (s *Store) Get(_ context.Context, id string) (payments.Payment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.byID[id]
	if !ok || s.expired(p, s.now()) {
		return payments.Payment{}, payments.ErrNotFound
	}
	return p, nil
}

// Len reports how many payments are held (after a sweep).
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweep(s.now())
	return len(s.byID)
}

func (s *Store) expired(p payments.Payment, now time.Time) bool {
	return !p.CreatedAt.Add(s.ttl).After(now)
}

// sweep drops expired payments. It runs lazily on writes, at most once per
// tenth of the TTL unless the store is full, so a hot path never scans a
// large map on every call. The caller holds s.mu.
func (s *Store) sweep(now time.Time) {
	if len(s.byID) < s.max && now.Sub(s.lastSweep) < s.ttl/10 {
		return
	}
	s.lastSweep = now
	for key, id := range s.byKey {
		if p, ok := s.byID[id]; !ok || s.expired(p, now) {
			delete(s.byKey, key)
			delete(s.byID, id)
		}
	}
}
