package app_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/eliteGoblin/eks-gitops-demo/internal/adapter/postgres/pgtest"
	"github.com/eliteGoblin/eks-gitops-demo/internal/app"
	"github.com/eliteGoblin/eks-gitops-demo/internal/domain"
)

// node must never be a way around the typed queries: each role sees exactly
// what participant(id), plan(id) and the claim listing show it.
func TestNodeReturnsOnlyWhatTheTypedQueriesReturn(t *testing.T) {
	forEachStore(t, func(t *testing.T, env testEnv) {
		sr, err := env.svc.SupportRequests.Raise(as(app.RoleParticipant, "P1"), app.RaiseSupportRequestInput{
			PlanID: "PL1", Category: domain.CategoryTherapy, Description: "Weekly physiotherapy", EstimatedAmount: 100_00,
		})
		if err != nil {
			t.Fatal(err)
		}
		cases := []struct {
			name            string
			ctx             context.Context
			visible, hidden []string
		}{
			{"participant P1", as(app.RoleParticipant, "P1"), []string{"P1", "PL1", "C1", "C2", "PR1", "PR2", sr.ID}, []string{"P2", "PL2", "C3", "nope"}},
			{"nominee for P1", as(app.RoleNominee, "N1"), []string{"P1", "PL1", "C1", sr.ID}, []string{"P2", "PL3", "C3"}},
			{"participant P3", as(app.RoleParticipant, "P3"), []string{"P3", "PL3", "PR1"}, []string{"P1", "PL1", "C1", sr.ID}},
			{"provider PR1", as(app.RoleProvider, "PR1"), []string{"P1", "PL1", "C1", "C2", "PR2"}, []string{"P2", "P3", "PL2", "PL3", "C3", sr.ID}},
			{"staff", as(app.RoleAgencyStaff, ""), []string{"P1", "P2", "P3", "PL1", "PL2", "PL3", "C1", "C2", "C3", "PR1", "PR2", sr.ID}, []string{"nope", ""}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				for _, id := range tc.visible {
					if rec, err := env.svc.Nodes.Get(tc.ctx, id); err != nil || rec == nil {
						t.Errorf("node(%q) = %+v, %v; want the record", id, rec, err)
					}
				}
				for _, id := range tc.hidden {
					if rec, err := env.svc.Nodes.Get(tc.ctx, id); err != nil || rec != nil {
						t.Errorf("node(%q) = %+v, %v; want nil", id, rec, err)
					}
				}
			})
		}
	})
}

func TestNodeAppliesFieldMasking(t *testing.T) {
	svc, _, _ := newServices(t)
	for role, wantDOB := range map[app.Role]bool{app.RoleProvider: false, app.RoleAgencyStaff: true} {
		id := ""
		if role == app.RoleProvider {
			id = "PR1"
		}
		rec, err := svc.Nodes.Get(as(role, id), "P1")
		if err != nil || rec == nil || rec.Participant == nil {
			t.Fatalf("%s: node(P1) = %+v, %v", role, rec, err)
		}
		if got := rec.Participant.DateOfBirth != nil; got != wantDOB {
			t.Errorf("%s: dateOfBirth visible = %v, want %v", role, got, wantDOB)
		}
	}
}

func TestNodesKeepsTheOrderOfIDs(t *testing.T) {
	svc, _, _ := newServices(t)
	ctx := as(app.RoleParticipant, "P1")
	recs, err := svc.Nodes.GetMany(ctx, []string{"C1", "P2", "P1", "nope"})
	if err != nil || len(recs) != 4 {
		t.Fatalf("GetMany: %v %v", recs, err)
	}
	if recs[0] == nil || recs[0].Claim == nil || recs[0].Claim.ID != "C1" || recs[1] != nil || recs[2] == nil || recs[2].Participant == nil || recs[3] != nil {
		t.Fatalf("GetMany alignment: %+v", recs)
	}
	if _, err := svc.Nodes.GetMany(ctx, make([]string, app.MaxNodes+1)); !errors.Is(err, domain.ErrInvalidInput) {
		t.Fatalf("too many ids: %v", err)
	}
	if _, err := svc.Nodes.Get(context.Background(), "P1"); !errors.Is(err, app.ErrUnauthenticated) {
		t.Fatalf("no caller: %v", err)
	}
}

type found struct{ participants, providers, claims []string }

func idsOf(r app.SearchResults) found {
	f := found{participants: []string{}, providers: []string{}, claims: []string{}}
	for _, p := range r.Participants {
		f.participants = append(f.participants, p.ID)
	}
	for _, p := range r.Providers {
		f.providers = append(f.providers, p.ID)
	}
	for _, c := range r.Claims {
		f.claims = append(f.claims, c.ID)
	}
	return f
}

func TestSearchStaysWithinWhatTheCallerMayRead(t *testing.T) {
	forEachStore(t, func(t *testing.T, env testEnv) {
		staff := as(app.RoleAgencyStaff, "")
		none := []string{}
		cases := []struct {
			name  string
			ctx   context.Context
			term  string
			first int
			want  found
		}{
			{"name, any case", staff, "sun", 10, found{[]string{"P1"}, []string{"PR1"}, none}},
			{"member number, by family name", staff, "SN10000", 10, found{[]string{"P3", "P2", "P1"}, none, none}},
			{"first caps the total", staff, "SN10000", 2, found{[]string{"P3", "P2"}, none, none}},
			{"provider sees booked participants only", as(app.RoleProvider, "PR1"), "SN10000", 10, found{[]string{"P1"}, none, none}},
			{"participant sees itself only", as(app.RoleParticipant, "P3"), "sun", 10, found{none, []string{"PR1"}, none}},
			{"provider by ABN", staff, "98765", 10, found{none, []string{"PR2"}, none}},
			{"claim by id prefix", staff, "c3", 10, found{none, none, []string{"C3"}}},
			{"claims outside the scope", as(app.RoleProvider, "PR1"), "C3", 10, found{none, none, none}},
			{"wildcards match literally", staff, "%%", 10, found{none, none, none}},
			{"underscore matches literally", staff, "S_n", 10, found{none, none, none}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				res, err := env.svc.Search.Search(tc.ctx, tc.term, tc.first)
				if err != nil {
					t.Fatal(err)
				}
				got := idsOf(res)
				if !slices.Equal(got.participants, tc.want.participants) || !slices.Equal(got.providers, tc.want.providers) || !slices.Equal(got.claims, tc.want.claims) {
					t.Errorf("search %q = %+v, want %+v", tc.term, got, tc.want)
				}
			})
		}
	})
}

func TestSearchMasksAuditsAndValidates(t *testing.T) {
	svc, audit, _ := newServices(t)
	res, err := svc.Search.Search(as(app.RoleProvider, "PR1"), "sun", 10)
	if err != nil || len(res.Participants) != 1 || res.Participants[0].DateOfBirth != nil {
		t.Fatalf("provider search must mask dateOfBirth: %+v %v", res.Participants, err)
	}
	if !audit.has(app.ActionSearch, "allow", "1 participants, 1 providers, 0 claims") {
		t.Fatalf("search not audited once with counts: %+v", audit.events)
	}
	for _, term := range []string{"a", "  b  ", strings.Repeat("x", 101)} {
		if _, err := svc.Search.Search(as(app.RoleAgencyStaff, ""), term, 10); !errors.Is(err, domain.ErrInvalidInput) {
			t.Errorf("term %q: %v", term, err)
		}
	}
	for _, first := range []int{0, app.MaxSearchResults + 1} {
		if _, err := svc.Search.Search(as(app.RoleAgencyStaff, ""), "sun", first); !errors.Is(err, domain.ErrInvalidInput) {
			t.Errorf("first %d: %v", first, err)
		}
	}
	if _, err := svc.Search.Search(context.Background(), "sun", 10); !errors.Is(err, app.ErrUnauthenticated) {
		t.Errorf("no caller: %v", err)
	}
}

// walk lists every claim the caller may read in order o, one per page,
// following the cursors the GraphQL adapter would hand out.
func walk(t *testing.T, svc *app.Services, ctx context.Context, f app.ClaimFilter, o app.ClaimOrder) []string {
	t.Helper()
	ids := []string{}
	page := app.Page{First: 1, Order: o}
	for range 20 {
		res, err := svc.Claims.List(ctx, f, page)
		if err != nil {
			t.Fatalf("list %+v: %v", o, err)
		}
		for _, c := range res.Claims {
			ids = append(ids, c.ID)
		}
		if !res.HasNext {
			return ids
		}
		cur := app.NewClaimCursor(res.Claims[len(res.Claims)-1], o)
		page.After = &cur
	}
	t.Fatal("too many pages")
	return nil
}

func TestClaimListingsPageThroughEveryOrder(t *testing.T) {
	forEachStore(t, func(t *testing.T, env testEnv) {
		// Same amount as C2, so AMOUNT orders need the id tie-break.
		c, err := env.svc.Claims.Submit(as(app.RoleProvider, "PR1"), app.SubmitClaimInput{
			PlanID: "PL1", Category: domain.CategoryDailyLiving, Amount: 180_50, ServiceDate: "2026-04-20",
		})
		if err != nil {
			t.Fatal(err)
		}
		n := c.ID // a UUIDv7: sorts before "C2" byte-wise
		staff := as(app.RoleAgencyStaff, "")
		for _, tc := range []struct {
			order app.ClaimOrder
			want  []string
		}{
			{app.ClaimOrder{}, []string{"C1", "C2", "C3", n}},
			{app.ClaimOrder{Field: app.OrderBySubmittedAt, Descending: true}, []string{n, "C3", "C2", "C1"}},
			{app.ClaimOrder{Field: app.OrderByAmount}, []string{n, "C2", "C1", "C3"}},
			{app.ClaimOrder{Field: app.OrderByAmount, Descending: true}, []string{"C3", "C1", "C2", n}},
		} {
			if got := walk(t, env.svc, staff, app.ClaimFilter{}, tc.order); !slices.Equal(got, tc.want) {
				t.Errorf("order %+v: %v, want %v", tc.order, got, tc.want)
			}
		}
		res, err := env.svc.Claims.List(staff, app.ClaimFilter{}, app.Page{First: 1, Order: app.ClaimOrder{Field: app.OrderByAmount}})
		if err != nil || res.Total != 4 {
			t.Fatalf("first page: %+v %v", res, err)
		}
		amountCursor := app.NewClaimCursor(res.Claims[0], app.ClaimOrder{Field: app.OrderByAmount})
		if _, err := env.svc.Claims.List(staff, app.ClaimFilter{}, app.Page{First: 1, After: &amountCursor}); !errors.Is(err, domain.ErrInvalidInput) {
			t.Fatalf("a cursor used with another order must be rejected: %v", err)
		}
	})
}

func TestClaimFiltersCombine(t *testing.T) {
	forEachStore(t, func(t *testing.T, env testEnv) {
		staff := as(app.RoleAgencyStaff, "")
		march := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
		april := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
		submitted := domain.ClaimSubmitted
		therapy := domain.CategoryTherapy
		for name, tc := range map[string]struct {
			ctx  context.Context
			f    app.ClaimFilter
			want []string
		}{
			"provider":          {staff, app.ClaimFilter{ProviderID: "PR2"}, []string{"C3"}},
			"category":          {staff, app.ClaimFilter{Category: &therapy}, []string{}},
			"submitted window":  {staff, app.ClaimFilter{SubmittedAfter: &march, SubmittedBefore: &april}, []string{"C2"}},
			"after inclusive":   {staff, app.ClaimFilter{SubmittedAfter: ptr(time.Date(2026, 3, 4, 10, 30, 0, 0, time.UTC))}, []string{"C2", "C3"}},
			"status and plan":   {staff, app.ClaimFilter{PlanID: "PL1", Status: &submitted}, []string{"C2"}},
			"scope still holds": {as(app.RoleProvider, "PR1"), app.ClaimFilter{ProviderID: "PR2"}, []string{}},
		} {
			if got := walk(t, env.svc, tc.ctx, tc.f, app.ClaimOrder{}); !slices.Equal(got, tc.want) {
				t.Errorf("%s: %v, want %v", name, got, tc.want)
			}
		}
	})
}

func ptr[T any](v T) *T { return &v }

func retryInput(key string) app.SubmitClaimInput {
	return app.SubmitClaimInput{PlanID: "PL1", Category: domain.CategoryDailyLiving, Amount: 12_00, ServiceDate: "2026-04-20", IdempotencyKey: key}
}

func claimsOn(t *testing.T, env testEnv, planID string) int {
	t.Helper()
	claims, err := env.store.ClaimsOfPlan(context.Background(), planID)
	if err != nil {
		t.Fatal(err)
	}
	return len(claims)
}

func TestARetryWithTheSameKeyReturnsTheFirstClaim(t *testing.T) {
	forEachStore(t, func(t *testing.T, env testEnv) {
		ctx := as(app.RoleProvider, "PR1")
		before := claimsOn(t, env, "PL1")
		first, err := env.svc.Claims.Submit(ctx, retryInput("retry-1"))
		if err != nil {
			t.Fatal(err)
		}
		again, err := env.svc.Claims.Submit(ctx, retryInput("retry-1"))
		if err != nil || again.ID != first.ID {
			t.Fatalf("retry: %+v %v, want claim %s", again, err, first.ID)
		}
		if got := claimsOn(t, env, "PL1"); got != before+1 {
			t.Fatalf("claims on PL1: %d, want %d", got, before+1)
		}
		if !env.audit.has(app.ActionSubmitClaim, "replayed", "") {
			t.Fatal("the replay is not audited")
		}
		changed := retryInput("retry-1")
		changed.Amount = 13_00
		var reused *app.IdempotencyKeyReusedError
		if _, err := env.svc.Claims.Submit(ctx, changed); !errors.As(err, &reused) {
			t.Fatalf("same key, different input: %v", err)
		}
		other := app.WithPrincipal(context.Background(), app.Principal{Subject: "another-user", Roles: map[app.Role]bool{app.RoleProvider: true}, ProviderID: "PR1"})
		if c, err := env.svc.Claims.Submit(other, retryInput("retry-1")); err != nil || c.ID == first.ID {
			t.Fatalf("keys are scoped to the caller: %+v %v", c, err)
		}
	})
}

func TestConcurrentRetriesStoreOneClaim(t *testing.T) {
	forEachStore(t, func(t *testing.T, env testEnv) {
		ctx := as(app.RoleProvider, "PR1")
		before := claimsOn(t, env, "PL1")
		const retries = 20
		ids := make([]string, retries)
		errs := make([]error, retries)
		var wg sync.WaitGroup
		for i := range retries {
			wg.Add(1)
			go func() {
				defer wg.Done()
				c, err := env.svc.Claims.Submit(ctx, retryInput("concurrent"))
				ids[i], errs[i] = c.ID, err
			}()
		}
		wg.Wait()
		for i := range retries {
			if errs[i] != nil || ids[i] != ids[0] {
				t.Fatalf("retry %d: %s %v, want %s", i, ids[i], errs[i], ids[0])
			}
		}
		if got := claimsOn(t, env, "PL1"); got != before+1 {
			t.Fatalf("claims on PL1 after %d concurrent retries: %d, want %d", retries, got, before+1)
		}
	})
}

func TestIdempotencyKeysAreValidated(t *testing.T) {
	svc, _, _ := newServices(t)
	for _, key := range []string{"has space", "semi;colon", strings.Repeat("k", 129)} {
		var ve *domain.ValidationError
		if _, err := svc.Claims.Submit(as(app.RoleProvider, "PR1"), retryInput(key)); !errors.As(err, &ve) || ve.Field != "idempotencyKey" {
			t.Errorf("key %q: %v", key, err)
		}
	}
}

// Ids and search terms are client input: a NUL byte, invalid UTF-8 or an
// absurd length is a bad request on every store, never a database error.
func TestMalformedIDsAndTermsAreInvalidInput(t *testing.T) {
	forEachStore(t, func(t *testing.T, env testEnv) {
		ctx := as(app.RoleAgencyStaff, "")
		for _, id := range []string{"a\x00b", "\xff", strings.Repeat("x", 129)} {
			if _, err := env.svc.Nodes.Get(ctx, id); !errors.Is(err, domain.ErrInvalidInput) {
				t.Errorf("node(%q): %v, want ErrInvalidInput", id, err)
			}
			if _, err := env.svc.Nodes.GetMany(ctx, []string{"P1", id}); !errors.Is(err, domain.ErrInvalidInput) {
				t.Errorf("nodes([P1 %q]): %v, want ErrInvalidInput for the batch", id, err)
			}
		}
		for _, term := range []string{"a\x00b", "su\xffn"} {
			if _, err := env.svc.Search.Search(ctx, term, 10); !errors.Is(err, domain.ErrInvalidInput) {
				t.Errorf("search(%q): %v, want ErrInvalidInput", term, err)
			}
		}
		if rec, err := env.svc.Nodes.Get(ctx, strings.Repeat("x", 128)); err != nil || rec != nil {
			t.Errorf("a well-formed unknown id is not an error: %+v %v", rec, err)
		}
	})
}

// A submission the budget refuses stores nothing: no claim and no key, so
// the key is still free and a corrected retry under it goes through.
func TestARefusedKeyedSubmissionStoresNothing(t *testing.T) {
	forEachStore(t, func(t *testing.T, env testEnv) {
		ctx := as(app.RoleProvider, "PR1")
		before := claimsOn(t, env, "PL1")
		tooMuch := retryInput("refused-then-fixed")
		tooMuch.Amount = 99_999_00
		var be *domain.BudgetExceededError
		if _, err := env.svc.Claims.Submit(ctx, tooMuch); !errors.As(err, &be) {
			t.Fatalf("over budget: %v, want BudgetExceededError", err)
		}
		if got := claimsOn(t, env, "PL1"); got != before {
			t.Fatalf("a refused submission stored a claim: %d claims, want %d", got, before)
		}
		fixed, err := env.svc.Claims.Submit(ctx, retryInput("refused-then-fixed"))
		if err != nil {
			t.Fatalf("the refused request must not have used the key: %v", err)
		}
		again, err := env.svc.Claims.Submit(ctx, retryInput("refused-then-fixed"))
		if err != nil || again.ID != fixed.ID {
			t.Fatalf("a retry of the accepted request replays it: %+v %v, want %s", again, err, fixed.ID)
		}
		if got := claimsOn(t, env, "PL1"); got != before+1 {
			t.Fatalf("claims on PL1: %d, want %d", got, before+1)
		}
	})
}

// A cursor of the amount order must carry the claim's amount: one that does
// not (tampered, or from another listing) is rejected, not trusted.
func TestAnAmountCursorMustMatchTheClaim(t *testing.T) {
	forEachStore(t, func(t *testing.T, env testEnv) {
		c1, err := env.store.GetClaim(context.Background(), "C1")
		if err != nil {
			t.Fatal(err)
		}
		byAmount := app.ClaimOrder{Field: app.OrderByAmount}
		ctx := as(app.RoleAgencyStaff, "")
		page := func(amount domain.Money) error {
			_, err := env.svc.Claims.List(ctx, app.ClaimFilter{}, app.Page{First: 2, Order: byAmount,
				After: &app.ClaimCursor{Order: byAmount, Amount: amount, ID: "C1"}})
			return err
		}
		if err := page(c1.Amount); err != nil {
			t.Fatalf("the claim's own amount: %v", err)
		}
		if err := page(c1.Amount + 1); !errors.Is(err, domain.ErrInvalidInput) {
			t.Fatalf("another amount: %v, want ErrInvalidInput", err)
		}
	})
}

// statementCounter counts the statements pgx sends.
type statementCounter struct{ n atomic.Int64 }

func (c *statementCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	c.n.Add(1)
	return ctx
}

func (c *statementCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// On Postgres, nodes(50) is a dozen statements: a batch read per record type
// (plans with their allocations), not one probe per id per type.
func TestNodesCostAFewStatementsOnPostgres(t *testing.T) {
	counter := &statementCounter{}
	st := pgtest.NewStore(t, func(cfg *pgxpool.Config) { cfg.ConnConfig.Tracer = counter })
	env := newEnv(t, st)
	pool := []string{"C1", "C2", "C3", "PL1", "PL2", "PL3", "P1", "P2", "P3", "PR1", "PR2", "nope"}
	ids := make([]string, app.MaxNodes)
	for i := range ids {
		ids[i] = pool[i%len(pool)]
	}
	counter.n.Store(0)
	recs, err := env.svc.Nodes.GetMany(as(app.RoleAgencyStaff, ""), ids)
	if err != nil || len(recs) != len(ids) {
		t.Fatalf("GetMany: %v %v", recs, err)
	}
	if n := counter.n.Load(); n > 15 {
		t.Fatalf("nodes(%d) sent %d statements, want at most 15 (one batch per record type)", len(ids), n)
	}
}
