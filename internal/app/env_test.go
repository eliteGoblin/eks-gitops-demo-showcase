package app_test

import (
	"context"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/eliteGoblin/eks-gitops-demo/internal/adapter/memory"
	"github.com/eliteGoblin/eks-gitops-demo/internal/adapter/postgres/pgtest"
	"github.com/eliteGoblin/eks-gitops-demo/internal/app"
)

type recordingAuditor struct {
	mu     sync.Mutex
	events []app.AuditEvent
}

func (r *recordingAuditor) Record(_ context.Context, e app.AuditEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *recordingAuditor) has(a app.Action, outcome, detail string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.ContainsFunc(r.events, func(e app.AuditEvent) bool {
		return e.Action == a && e.Outcome == outcome && e.Detail == detail
	})
}

func (r *recordingAuditor) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = nil
}

// store is every port one adapter implements.
type store interface {
	app.ParticipantRepository
	app.PlanRepository
	app.ProviderRepository
	app.ClaimRepository
	app.BookingRepository
	app.SupportRequestRepository
	app.Relationships
}

type testEnv struct {
	svc   *app.Services
	audit *recordingAuditor
	store store
}

var testNow = time.Date(2026, 5, 2, 0, 0, 0, 0, time.UTC)

func newEnv(t *testing.T, st store) testEnv {
	t.Helper()
	return newEnvWith(t, st, nil, nil)
}

// newEnvWith wires optional payments and events (nil keeps them off).
func newEnvWith(t *testing.T, st store, payments app.PaymentGateway, events app.Events) testEnv {
	t.Helper()
	audit := &recordingAuditor{}
	svc, err := app.NewServices(app.Deps{
		Participants: st, Plans: st, Providers: st, Claims: st, Bookings: st, SupportRequests: st,
		Relationships: st, Auditor: audit,
		Clock:    func() time.Time { return testNow },
		NextID:   func() string { return uuid.Must(uuid.NewV7()).String() },
		Payments: payments,
		Events:   events,
	})
	if err != nil {
		t.Fatal(err)
	}
	return testEnv{svc: svc, audit: audit, store: st}
}

// newServices runs on the seeded memory store.
func newServices(t *testing.T) (*app.Services, *recordingAuditor, store) {
	t.Helper()
	st := memory.New()
	memory.Seed(st)
	env := newEnv(t, st)
	return env.svc, env.audit, env.store
}

// forEachStore runs a contract test against the memory store and, when
// TEST_DATABASE_URL is set, against Postgres (a fresh, migrated and seeded
// schema per test), so both adapters honour the same behaviour.
func forEachStore(t *testing.T, test func(t *testing.T, env testEnv)) {
	t.Helper()
	t.Run("memory", func(t *testing.T) {
		st := memory.New()
		memory.Seed(st)
		test(t, newEnv(t, st))
	})
	if os.Getenv(pgtest.EnvDSN) == "" {
		return
	}
	t.Run("postgres", func(t *testing.T) {
		test(t, newEnv(t, pgtest.NewStore(t)))
	})
}
