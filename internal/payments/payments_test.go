package payments_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eliteGoblin/eks-gitops-demo/internal/payments"
	"github.com/eliteGoblin/eks-gitops-demo/internal/payments/memstore"
)

var fixedNow = time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)

func newService() *payments.Service {
	var seq atomic.Int64
	clock := func() time.Time { return fixedNow }
	// The store gets the same fixed clock: on the wall clock, a payment stamped
	// fixedNow is past its retention (memstore.DefaultTTL) a day later.
	return payments.NewService(memstore.NewWithOptions(memstore.Options{Now: clock}), clock,
		func() string { return fmt.Sprintf("pay-%d", seq.Add(1)) })
}

func asCaller(client string, perms ...payments.Permission) context.Context {
	return payments.WithCaller(context.Background(), payments.Caller{Subject: "sa-" + client, ClientID: client, Permissions: perms})
}

var validRequest = payments.Request{Reference: "C101", Payee: "PR1", AmountCents: 12050, Currency: "AUD"}

func TestCreateAndGet(t *testing.T) {
	svc := newService()
	ctx := asCaller("demo-api-svc", payments.PermWrite)
	p, replayed, err := svc.Create(ctx, "key-1", validRequest)
	if err != nil || replayed {
		t.Fatalf("create: %+v replayed=%v err=%v", p, replayed, err)
	}
	if p.ID != "pay-1" || p.Status != payments.StatusAccepted || !p.CreatedAt.Equal(fixedNow) || p.Request != validRequest {
		t.Fatalf("payment = %+v", p)
	}
	got, err := svc.Get(ctx, p.ID)
	if err != nil || got != p {
		t.Fatalf("get = %+v, %v", got, err)
	}
}

func TestCreateIsIdempotentPerKey(t *testing.T) {
	svc := newService()
	ctx := asCaller("demo-api-svc", payments.PermWrite)
	first, _, _ := svc.Create(ctx, "key-1", validRequest)

	again, replayed, err := svc.Create(ctx, "key-1", validRequest)
	if err != nil || !replayed || again != first {
		t.Fatalf("a retry with the same key must return the same payment: %+v replayed=%v err=%v", again, replayed, err)
	}

	changed := validRequest
	changed.AmountCents = 99
	if _, _, err := svc.Create(ctx, "key-1", changed); !errors.Is(err, payments.ErrKeyReused) {
		t.Fatalf("the same key with a different request must be refused, got %v", err)
	}

	other, replayed, err := svc.Create(asCaller("other-svc", payments.PermWrite), "key-1", validRequest)
	if err != nil || replayed || other.ID == first.ID {
		t.Fatalf("keys are scoped per client: %+v replayed=%v err=%v", other, replayed, err)
	}
}

func TestConcurrentRetriesCreateOnePayment(t *testing.T) {
	svc := newService()
	ctx := asCaller("demo-api-svc", payments.PermWrite)
	var wg sync.WaitGroup
	ids := make(chan string, 50)
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, _, err := svc.Create(ctx, "same-key", validRequest)
			if err != nil {
				t.Error(err)
				return
			}
			ids <- p.ID
		}()
	}
	wg.Wait()
	close(ids)
	seen := map[string]bool{}
	for id := range ids {
		seen[id] = true
	}
	if len(seen) != 1 {
		t.Fatalf("50 concurrent retries created %d payments, want 1", len(seen))
	}
}

func TestCreateRejects(t *testing.T) {
	tests := []struct {
		name      string
		ctx       context.Context
		key       string
		req       payments.Request
		wantErr   error
		wantField string
	}{
		{"no caller", context.Background(), "k", validRequest, payments.ErrForbidden, ""},
		{"caller without payments:write", asCaller("demo-api-svc", payments.PermRead), "k", validRequest, payments.ErrForbidden, ""},
		{"missing idempotency key", asCaller("c", payments.PermWrite), "", validRequest, nil, "Idempotency-Key"},
		{"over-long idempotency key", asCaller("c", payments.PermWrite), string(make([]byte, 256)), validRequest, nil, "Idempotency-Key"},
		{"missing reference", asCaller("c", payments.PermWrite), "k", with(func(r *payments.Request) { r.Reference = "" }), nil, "reference"},
		{"missing payee", asCaller("c", payments.PermWrite), "k", with(func(r *payments.Request) { r.Payee = "" }), nil, "payee"},
		{"zero amount", asCaller("c", payments.PermWrite), "k", with(func(r *payments.Request) { r.AmountCents = 0 }), nil, "amountCents"},
		{"negative amount", asCaller("c", payments.PermWrite), "k", with(func(r *payments.Request) { r.AmountCents = -5 }), nil, "amountCents"},
		{"amount over the limit", asCaller("c", payments.PermWrite), "k", with(func(r *payments.Request) { r.AmountCents = payments.MaxAmountCents + 1 }), nil, "amountCents"},
		{"unsupported currency", asCaller("c", payments.PermWrite), "k", with(func(r *payments.Request) { r.Currency = "USD" }), nil, "currency"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := newService().Create(tc.ctx, tc.key, tc.req)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("want %v, got %v", tc.wantErr, err)
				}
				return
			}
			var ve *payments.ValidationError
			if !errors.As(err, &ve) || ve.Field != tc.wantField {
				t.Fatalf("want a validation error on %q, got %v", tc.wantField, err)
			}
		})
	}
}

func with(change func(*payments.Request)) payments.Request {
	r := validRequest
	change(&r)
	return r
}

func TestGetRules(t *testing.T) {
	svc := newService()
	p, _, _ := svc.Create(asCaller("demo-api-svc", payments.PermWrite), "k", validRequest)

	if _, err := svc.Get(asCaller("demo-api-svc", payments.PermRead), p.ID); err != nil {
		t.Fatalf("payments:read may read its own client's payment: %v", err)
	}
	if _, err := svc.Get(asCaller("other-svc", payments.PermWrite), p.ID); !errors.Is(err, payments.ErrNotFound) {
		t.Fatalf("another client's payment must look not found, got %v", err)
	}
	if _, err := svc.Get(asCaller("demo-api-svc", payments.PermWrite), "pay-missing"); !errors.Is(err, payments.ErrNotFound) {
		t.Fatalf("unknown id: %v", err)
	}
	if _, err := svc.Get(asCaller("demo-api-svc"), p.ID); !errors.Is(err, payments.ErrForbidden) {
		t.Fatalf("a caller without permissions must be forbidden, got %v", err)
	}
}

func TestValidationErrorMessage(t *testing.T) {
	err := &payments.ValidationError{Field: "payee", Message: "is required"}
	if err.Error() != "payee: is required" {
		t.Fatalf("got %q", err.Error())
	}
}
