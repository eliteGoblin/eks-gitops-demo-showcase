package memstore

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/eliteGoblin/eks-gitops-demo/internal/payments"
)

func TestCreateOnceAndGet(t *testing.T) {
	s := New()
	ctx := context.Background()
	first, created, err := s.CreateOnce(ctx, "k", payments.Payment{ID: "p1", CreatedAt: time.Now()})
	if err != nil || !created || first.ID != "p1" {
		t.Fatalf("first create: %+v %v %v", first, created, err)
	}
	again, created, _ := s.CreateOnce(ctx, "k", payments.Payment{ID: "p2", CreatedAt: time.Now()})
	if created || again.ID != "p1" {
		t.Fatalf("the key must keep its first payment: %+v created=%v", again, created)
	}
	if _, err := s.Get(ctx, "p2"); !errors.Is(err, payments.ErrNotFound) {
		t.Fatalf("the losing candidate must not be stored, got %v", err)
	}
	if got, err := s.Get(ctx, "p1"); err != nil || got.ID != "p1" {
		t.Fatalf("get: %+v %v", got, err)
	}
}

// A payment and its idempotency key are forgotten after the TTL: the key can
// then be used again, and the old id no longer resolves.
func TestPaymentsExpireAfterTheTTL(t *testing.T) {
	now := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	s := NewWithOptions(Options{TTL: time.Hour, MaxPayments: 10, Now: clock})
	ctx := context.Background()
	if _, _, err := s.CreateOnce(ctx, "k", payments.Payment{ID: "p1", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(59 * time.Minute)
	if _, err := s.Get(ctx, "p1"); err != nil {
		t.Fatalf("still within the TTL: %v", err)
	}
	now = now.Add(2 * time.Minute)
	if _, err := s.Get(ctx, "p1"); !errors.Is(err, payments.ErrNotFound) {
		t.Fatalf("expired payment must be gone, got %v", err)
	}
	p, created, err := s.CreateOnce(ctx, "k", payments.Payment{ID: "p2", CreatedAt: now})
	if err != nil || !created || p.ID != "p2" {
		t.Fatalf("an expired key must be reusable: %+v created=%v err=%v", p, created, err)
	}
	if n := s.Len(); n != 1 {
		t.Fatalf("the sweep must have dropped the expired payment, len=%d", n)
	}
}

// The store refuses new payments at the cap instead of growing without bound;
// replays of known keys still work, and room comes back as payments expire.
func TestStoreRefusesNewPaymentsWhenFull(t *testing.T) {
	now := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	s := NewWithOptions(Options{TTL: time.Hour, MaxPayments: 3, Now: clock})
	ctx := context.Background()
	for i := range 3 {
		if _, _, err := s.CreateOnce(ctx, fmt.Sprintf("k%d", i), payments.Payment{ID: fmt.Sprintf("p%d", i), CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := s.CreateOnce(ctx, "k-new", payments.Payment{ID: "p-new", CreatedAt: now}); !errors.Is(err, payments.ErrStoreFull) {
		t.Fatalf("a fourth payment must be refused with ErrStoreFull, got %v", err)
	}
	if p, created, err := s.CreateOnce(ctx, "k1", payments.Payment{ID: "dup", CreatedAt: now}); err != nil || created || p.ID != "p1" {
		t.Fatalf("a replay of a known key must still answer when full: %+v %v %v", p, created, err)
	}
	now = now.Add(2 * time.Hour)
	if _, created, err := s.CreateOnce(ctx, "k-new", payments.Payment{ID: "p-new", CreatedAt: now}); err != nil || !created {
		t.Fatalf("after the old payments expire there is room again: created=%v err=%v", created, err)
	}
}
