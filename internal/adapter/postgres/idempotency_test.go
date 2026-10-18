package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/eliteGoblin/eks-gitops-demo/internal/adapter/postgres"
	"github.com/eliteGoblin/eks-gitops-demo/internal/adapter/postgres/pgtest"
	"github.com/eliteGoblin/eks-gitops-demo/internal/app"
	"github.com/eliteGoblin/eks-gitops-demo/internal/domain"
)

// Two requests with one key on different plans hold different plan locks, so
// only the key's primary key orders them. Hold the first request open with
// the key inserted, start the second, wait until it blocks on the key, then
// commit the first: the second must roll its claim back and report the key
// as used (the unique-violation path, not the read-first path).
func TestSameKeyOnAnotherPlanWaitsAndReplays(t *testing.T) {
	st := pgtest.NewStore(t)
	ctx := context.Background()
	held, err := st.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Rollback(ctx) }()
	if _, err := held.Exec(ctx, `INSERT INTO claims (id, plan_id, provider_id, category, amount_cents, status, service_date, submitted_at)
		VALUES ('FIRST', 'PL2', 'PR2', 'DAILY_LIVING', 100, 'SUBMITTED', '2026-04-01', now())`); err != nil {
		t.Fatal(err)
	}
	if _, err := held.Exec(ctx, `INSERT INTO idempotency_keys (subject, key, fingerprint, claim_id) VALUES ('s', 'k', 'first', 'FIRST')`); err != nil {
		t.Fatal(err)
	}

	type outcome struct {
		prior    app.IdempotencyRecord
		replayed bool
		err      error
	}
	done := make(chan outcome, 1)
	second := domain.Claim{ID: "SECOND", PlanID: "PL1", ProviderID: "PR1", Category: domain.CategoryDailyLiving, Amount: 100,
		Status: domain.ClaimSubmitted, ServiceDate: "2026-04-20", SubmittedAt: time.Now().UTC()}
	go func() {
		prior, replayed, err := st.InsertClaimOnce(ctx, second, app.IdempotencyKey{Subject: "s", Key: "k", Fingerprint: "second"},
			func([]domain.Claim, []domain.ServiceBooking) error { return nil })
		done <- outcome{prior, replayed, err}
	}()
	waitForLockWait(t, st)
	if err := held.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	got := <-done
	if got.err != nil || !got.replayed || got.prior.ClaimID != "FIRST" || got.prior.Fingerprint != "first" {
		t.Fatalf("second request: %+v", got)
	}
	if _, err := st.GetClaim(ctx, "SECOND"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("the second request's claim must be rolled back, got %v", err)
	}
}

// waitForLockWait waits until the second request blocks on the key.
func waitForLockWait(t *testing.T, st *postgres.Store) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var waiting int
		err := st.Pool().QueryRow(context.Background(), `SELECT count(*) FROM pg_stat_activity
			WHERE wait_event_type = 'Lock' AND query LIKE '%INSERT INTO idempotency_keys%'`).Scan(&waiting)
		if err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the second request never waited on the key")
}
