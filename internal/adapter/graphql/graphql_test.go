package graphql_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eliteGoblin/eks-gitops-demo/internal/adapter/graphql"
	"github.com/eliteGoblin/eks-gitops-demo/internal/adapter/memory"
	"github.com/eliteGoblin/eks-gitops-demo/internal/app"
	"github.com/eliteGoblin/eks-gitops-demo/internal/domain"
)

type nopAudit struct{}

func (nopAudit) Record(context.Context, app.AuditEvent) {}

func server(t *testing.T) http.Handler {
	t.Helper()
	st := memory.New()
	memory.Seed(st)
	return serverOn(t, st)
}

// serverOn builds the handler on any store that implements every port.
func serverOn(t *testing.T, st store) http.Handler {
	t.Helper()
	return serverWith(t, st, nil)
}

// serverWith is serverOn with approvals paid through payments (nil: off).
func serverWith(t *testing.T, st store, payments app.PaymentGateway) http.Handler {
	t.Helper()
	return serverWithOptions(t, st, payments, nil)
}

// serverWithOptions is serverWith with handler options adjusted by tune (nil: defaults).
func serverWithOptions(t *testing.T, st store, payments app.PaymentGateway, tune func(*graphql.Options)) http.Handler {
	t.Helper()
	return serverAudited(t, st, payments, nopAudit{}, tune)
}

// serverAudited is serverWithOptions with every audit event sent to audit.
func serverAudited(t *testing.T, st store, payments app.PaymentGateway, audit app.Auditor, tune func(*graphql.Options)) http.Handler {
	t.Helper()
	var n atomic.Int64
	svc, err := app.NewServices(app.Deps{Participants: st, Plans: st, Providers: st, Claims: st, Bookings: st, SupportRequests: st,
		Relationships: st, Auditor: audit, Payments: payments,
		Clock: func() time.Time { return time.Date(2026, 5, 2, 0, 0, 0, 0, time.UTC) }, NextID: func() string { return fmt.Sprintf("N%d", n.Add(1)) }})
	if err != nil {
		t.Fatal(err)
	}
	opt := graphql.Options{Introspection: true, DepthLimit: 8, ComplexityLimit: 300, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if tune != nil {
		tune(&opt)
	}
	return graphql.NewHandler(svc, opt)
}

type store interface {
	app.ParticipantRepository
	app.PlanRepository
	app.ProviderRepository
	app.ClaimRepository
	app.BookingRepository
	app.SupportRequestRepository
	app.Relationships
}

type result struct {
	Data   map[string]json.RawMessage `json:"data"`
	Errors []struct {
		Message    string         `json:"message"`
		Extensions map[string]any `json:"extensions"`
	} `json:"errors"`
}

func do(t *testing.T, h http.Handler, pr *app.Principal, query string) result {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"query": query})
	req := httptest.NewRequest(http.MethodPost, "/graphql", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if pr != nil {
		req = req.WithContext(app.WithPrincipal(req.Context(), *pr))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var r result
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatalf("bad response %s", rec.Body)
	}
	return r
}

func code(r result) string {
	if len(r.Errors) == 0 {
		return ""
	}
	c, _ := r.Errors[0].Extensions["code"].(string)
	return c
}

var (
	participant = &app.Principal{Subject: "p", Roles: map[app.Role]bool{app.RoleParticipant: true}, ParticipantID: "P1"}
	provider    = &app.Principal{Subject: "r", Roles: map[app.Role]bool{app.RoleProvider: true}, ProviderID: "PR1"}
	staff       = &app.Principal{Subject: "s", Roles: map[app.Role]bool{app.RoleAgencyStaff: true}}
)

func TestQueriesAndMasking(t *testing.T) {
	h := server(t)
	r := do(t, h, participant, `{ me { roles participant { id schemeNumber dateOfBirth plans { id budgets { category remaining } } } } }`)
	if len(r.Errors) > 0 || !bytes.Contains(r.Data["me"], []byte(`"dateOfBirth":"1990-05-04"`)) || !bytes.Contains(r.Data["me"], []byte(`"roles":["PARTICIPANT"]`)) {
		t.Fatalf("me: %s %+v", r.Data["me"], r.Errors)
	}
	r = do(t, h, provider, `{ me { provider { name } } claims(first: 1) { totalCount pageInfo { hasNextPage endCursor } edges { node { id plan { participant { dateOfBirth } budgets { category } } provider { name } } } } }`)
	if len(r.Errors) > 0 || !bytes.Contains(r.Data["claims"], []byte(`"dateOfBirth":null`)) || !bytes.Contains(r.Data["claims"], []byte(`"hasNextPage":true`)) {
		t.Fatalf("provider claims: %s %+v", r.Data["claims"], r.Errors)
	}
	var page struct {
		PageInfo struct{ EndCursor string } `json:"pageInfo"`
	}
	_ = json.Unmarshal(r.Data["claims"], &page)
	r = do(t, h, provider, fmt.Sprintf(`{ claims(first: 5, after: %q) { edges { node { id } } } }`, page.PageInfo.EndCursor))
	if len(r.Errors) > 0 || !bytes.Contains(r.Data["claims"], []byte(`"C2"`)) {
		t.Fatalf("second page: %s %+v", r.Data["claims"], r.Errors)
	}
	r = do(t, h, participant, `{ participant(id: "P2") { id } plan(id: "PL1") { claims(status: APPROVED) { totalCount } } }`)
	if string(r.Data["participant"]) != "null" || !bytes.Contains(r.Data["plan"], []byte(`"totalCount":1`)) {
		t.Fatalf("hidden participant / filtered claims: %v %+v", r.Data, r.Errors)
	}
	if r := do(t, h, participant, `{ claims(first: 0) { totalCount } }`); code(r) != "BAD_USER_INPUT" {
		t.Fatalf("first: 0 must be BAD_USER_INPUT, got %+v", r.Errors)
	}
	if r := do(t, h, participant, `{ claims(after: "!!") { totalCount } }`); code(r) != "BAD_USER_INPUT" {
		t.Fatalf("bad cursor must be BAD_USER_INPUT, got %+v", r.Errors)
	}
	if r := do(t, h, nil, `{ me { subject } }`); code(r) != "UNAUTHENTICATED" {
		t.Fatalf("no principal: %+v", r.Errors)
	}
}

func TestMutations(t *testing.T) {
	h := server(t)
	submit := `mutation { submitClaim(input: {planId: "PL1", category: DAILY_LIVING, amount: "%s", serviceDate: "2026-05-01"}) { __typename ... on Claim { id status amount } ... on BudgetExceededError { remaining } ... on ValidationError { field } } }`
	tests := []struct {
		name string
		pr   *app.Principal
		q    string
		want string // substring of data, or error code prefixed with "!"
	}{
		{"valid claim", provider, fmt.Sprintf(submit, "120.50"), `"amount":"120.50"`},
		{"over budget", provider, fmt.Sprintf(submit, "999999.00"), `"BudgetExceededError"`},
		{"zero amount", provider, fmt.Sprintf(submit, "0"), `"field":"amount"`},
		{"bad money", provider, fmt.Sprintf(submit, "abc"), "!BAD_USER_INPUT"},
		{"participant cannot submit", participant, fmt.Sprintf(submit, "1.00"), "!FORBIDDEN"},
		{"provider cannot approve", provider, `mutation { approveClaim(id: "C2", reason: "x") { __typename } }`, "!FORBIDDEN"},
		{"staff approves", staff, `mutation { approveClaim(id: "C2", reason: "ok") { __typename ... on Claim { status } } }`, `"APPROVED"`},
		{"approve twice", staff, `mutation { approveClaim(id: "C2", reason: "ok") { __typename ... on ValidationError { message } } }`, `SUBMITTED state`},
		{"unknown plan", provider, `mutation { submitClaim(input: {planId: "X", category: DAILY_LIVING, amount: "1", serviceDate: "2026-05-01"}) { __typename } }`, "!FORBIDDEN"},
		{"bad date", provider, `mutation { submitClaim(input: {planId: "PL1", category: DAILY_LIVING, amount: 1.5, serviceDate: "2026-13-01"}) { __typename } }`, "!BAD_USER_INPUT"},
		{"introspection in dev", staff, `{ __schema { queryType { name } } }`, `"Query"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := do(t, h, tc.pr, tc.q)
			if tc.want[0] == '!' {
				if code(r) != tc.want[1:] {
					t.Fatalf("want error %s, got %+v data %v", tc.want[1:], r.Errors, r.Data)
				}
				return
			}
			all, _ := json.Marshal(r.Data)
			if !bytes.Contains(all, []byte(tc.want)) {
				t.Fatalf("want %s in %s (errors %+v)", tc.want, all, r.Errors)
			}
		})
	}
}

func TestStaleCursorIsRejected(t *testing.T) {
	h := server(t)
	for _, raw := range []string{"claim:2026-02-11T09:00:00Z|C999", "claim:C999", "claim:not-a-time|C1"} {
		cursor := base64.RawURLEncoding.EncodeToString([]byte(raw))
		r := do(t, h, staff, fmt.Sprintf(`{ claims(after: %q) { totalCount } }`, cursor))
		if code(r) != "BAD_USER_INPUT" {
			t.Fatalf("cursor %q must be BAD_USER_INPUT, not a restart from page 1; got %+v data %v", raw, r.Errors, r.Data)
		}
	}
}

func TestQueryLimits(t *testing.T) {
	h := server(t)
	tests := []struct {
		name string
		q    string
		code string // "" = must succeed
	}{
		{"normal nested query", `{ claims { edges { node { id plan { participant { dateOfBirth } budgets { category } } provider { name } } } } }`, ""},
		{"introspection is not depth-limited", `{ __schema { types { fields { type { ofType { ofType { ofType { ofType { ofType { name } } } } } } } } } }`, ""},
		{"too deep", `{ claims { edges { node { plan { participant { plans { claims { edges { node { plan { id } } } } } } } } } } }`, "DEPTH_LIMIT_EXCEEDED"},
		{"too deep through a fragment", `{ claims { edges { node { ...deep } } } } fragment deep on Claim { plan { participant { plans { claims { edges { node { id } } } } } } }`, "DEPTH_LIMIT_EXCEEDED"},
		{"too wide: first multiplies the cost", `{ claims(first: 100) { edges { node { id plan { id budgets { category } participant { id } } provider { name } } } } }`, "COMPLEXITY_LIMIT_EXCEEDED"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := do(t, h, staff, tc.q)
			if code(r) != tc.code {
				t.Fatalf("want code %q, got errors %+v", tc.code, r.Errors)
			}
		})
	}
}

// countingStore counts the single and batch reads behind nested fields.
type countingStore struct {
	*memory.Store
	getPlan, getPlans, getParticipant, getParticipants, getProvider, getProviders atomic.Int64
	getClaim, getClaims, getSupportRequest, getSupportRequests                    atomic.Int64
}

func (c *countingStore) GetClaim(ctx context.Context, id string) (domain.Claim, error) {
	c.getClaim.Add(1)
	return c.Store.GetClaim(ctx, id)
}

func (c *countingStore) GetClaims(ctx context.Context, ids []string) ([]domain.Claim, error) {
	c.getClaims.Add(1)
	return c.Store.GetClaims(ctx, ids)
}

func (c *countingStore) GetSupportRequest(ctx context.Context, id string) (domain.SupportRequest, error) {
	c.getSupportRequest.Add(1)
	return c.Store.GetSupportRequest(ctx, id)
}

func (c *countingStore) GetSupportRequests(ctx context.Context, ids []string) ([]domain.SupportRequest, error) {
	c.getSupportRequests.Add(1)
	return c.Store.GetSupportRequests(ctx, ids)
}

func (c *countingStore) GetPlan(ctx context.Context, id string) (domain.Plan, error) {
	c.getPlan.Add(1)
	return c.Store.GetPlan(ctx, id)
}

func (c *countingStore) GetPlans(ctx context.Context, ids []string) ([]domain.Plan, error) {
	c.getPlans.Add(1)
	return c.Store.GetPlans(ctx, ids)
}

func (c *countingStore) GetParticipant(ctx context.Context, id string) (domain.Participant, error) {
	c.getParticipant.Add(1)
	return c.Store.GetParticipant(ctx, id)
}

func (c *countingStore) GetParticipants(ctx context.Context, ids []string) ([]domain.Participant, error) {
	c.getParticipants.Add(1)
	return c.Store.GetParticipants(ctx, ids)
}

func (c *countingStore) GetProvider(ctx context.Context, id string) (domain.Provider, error) {
	c.getProvider.Add(1)
	return c.Store.GetProvider(ctx, id)
}

func (c *countingStore) GetProviders(ctx context.Context, ids []string) ([]domain.Provider, error) {
	c.getProviders.Add(1)
	return c.Store.GetProviders(ctx, ids)
}

// Nested fields over a list are loaded with one batch read per field level,
// however many items the list has (no N+1).
func TestNestedFieldsAreBatched(t *testing.T) {
	st := &countingStore{Store: memory.New()}
	memory.Seed(st.Store)
	h := serverOn(t, st)
	for range 4 { // more claims, same number of reads
		if r := do(t, h, provider, `mutation { submitClaim(input: {planId: "PL1", category: DAILY_LIVING, amount: "1.00", serviceDate: "2026-05-01"}) { __typename } }`); len(r.Errors) > 0 {
			t.Fatal(r.Errors)
		}
	}
	st.getPlan.Store(0)

	r := do(t, h, staff, `{ claims { totalCount edges { node { id plan { id participant { id } } provider { id } } } } }`)
	if len(r.Errors) > 0 || !bytes.Contains(r.Data["claims"], []byte(`"totalCount":7`)) {
		t.Fatalf("claims: %s %+v", r.Data["claims"], r.Errors)
	}
	reads := map[string]int64{
		"GetPlans": st.getPlans.Load(), "GetParticipants": st.getParticipants.Load(), "GetProviders": st.getProviders.Load(),
		"GetPlan": st.getPlan.Load(), "GetParticipant": st.getParticipant.Load(), "GetProvider": st.getProvider.Load(),
	}
	want := map[string]int64{"GetPlans": 1, "GetParticipants": 1, "GetProviders": 1, "GetPlan": 0, "GetParticipant": 0, "GetProvider": 0}
	for k, v := range want {
		if reads[k] != v {
			t.Errorf("%s called %d times, want %d (all reads: %v)", k, reads[k], v, reads)
		}
	}
}

func TestSupportRequestFlow(t *testing.T) {
	h := server(t)
	raise := `mutation { raiseSupportRequest(input: {planId: "%s", category: THERAPY, description: "Weekly physio", estimatedAmount: "%s", preferredProviderId: "PR2"}) {
		__typename ... on SupportRequest { id status requestedBy preferredProvider { name } plan { id } raisedAt } ... on ValidationError { field } } }`
	r := do(t, h, participant, fmt.Sprintf(raise, "PL1", "300.00"))
	var raised struct {
		Raise struct {
			Typename string `json:"__typename"`
			ID       string `json:"id"`
			Status   string `json:"status"`
		} `json:"raiseSupportRequest"`
	}
	all, _ := json.Marshal(r.Data)
	_ = json.Unmarshal(all, &raised)
	if raised.Raise.Typename != "SupportRequest" || raised.Raise.Status != "PENDING" || !bytes.Contains(all, []byte(`"Harbour Therapy"`)) {
		t.Fatalf("raise: %s %+v", all, r.Errors)
	}
	if r := do(t, h, participant, fmt.Sprintf(raise, "PL1", "0")); !bytes.Contains(r.Data["raiseSupportRequest"], []byte(`"field":"estimatedAmount"`)) {
		t.Fatalf("validation error as data: %s %+v", r.Data["raiseSupportRequest"], r.Errors)
	}
	if r := do(t, h, provider, fmt.Sprintf(raise, "PL1", "1")); code(r) != "FORBIDDEN" {
		t.Fatalf("providers cannot raise: %+v", r.Errors)
	}

	decide := `mutation { decideSupportRequest(input: {id: "%s", decision: %s, reason: "assessed"}) {
		__typename ... on SupportRequest { status bookedProvider { id } decisionReason decidedAt } ... on SelfApprovalNotAllowedError { message } ... on ValidationError { field } } }`
	if r := do(t, h, participant, fmt.Sprintf(decide, raised.Raise.ID, "APPROVE")); code(r) != "FORBIDDEN" {
		t.Fatalf("participants cannot decide: %+v", r.Errors)
	}
	r = do(t, h, staff, fmt.Sprintf(decide, raised.Raise.ID, "APPROVE"))
	if d := r.Data["decideSupportRequest"]; !bytes.Contains(d, []byte(`"status":"APPROVED"`)) || !bytes.Contains(d, []byte(`"id":"PR2"`)) {
		t.Fatalf("approve books the preferred provider: %s %+v", d, r.Errors)
	}
	if r := do(t, h, staff, `{ plan(id: "PL1") { budgets { category reserved } } }`); !bytes.Contains(r.Data["plan"], []byte(`{"category":"THERAPY","reserved":"300.00"}`)) {
		t.Fatalf("approval reserves the estimate: %s %+v", r.Data["plan"], r.Errors)
	}

	// Separation of duties: staff raise for a participant, then try to decide it.
	r = do(t, h, staff, fmt.Sprintf(raise, "PL3", "50.00"))
	all, _ = json.Marshal(r.Data)
	_ = json.Unmarshal(all, &raised)
	r = do(t, h, staff, fmt.Sprintf(decide, raised.Raise.ID, "REJECT"))
	if !bytes.Contains(r.Data["decideSupportRequest"], []byte(`"SelfApprovalNotAllowedError"`)) {
		t.Fatalf("own request: %s %+v", r.Data["decideSupportRequest"], r.Errors)
	}

	if r := do(t, h, staff, `{ pendingSupportRequests { id requestedBy } }`); !bytes.Contains(r.Data["pendingSupportRequests"], []byte(raised.Raise.ID)) {
		t.Fatalf("staff queue: %s %+v", r.Data["pendingSupportRequests"], r.Errors)
	}
	if r := do(t, h, participant, `{ pendingSupportRequests { id } }`); code(r) != "FORBIDDEN" {
		t.Fatalf("queue is staff-only: %+v", r.Errors)
	}
	if r := do(t, h, participant, `{ mySupportRequests { status bookedProvider { name } } }`); !bytes.Contains(r.Data["mySupportRequests"], []byte(`"APPROVED"`)) {
		t.Fatalf("participant sees their decided request: %s %+v", r.Data["mySupportRequests"], r.Errors)
	}
}

// gateway pays every request (ref "pay_" + key), or fails them all with err.
type gateway struct{ err error }

func (g gateway) RequestPayment(_ context.Context, r app.PaymentRequest) (app.PaymentReceipt, error) {
	if g.err != nil {
		return app.PaymentReceipt{}, g.err
	}
	return app.PaymentReceipt{PaymentID: "pay_" + r.IdempotencyKey, Status: "ACCEPTED"}, nil
}

func TestApprovedClaimShowsItsPayment(t *testing.T) {
	for name, tc := range map[string]struct {
		gateway app.PaymentGateway
		want    string
	}{
		"paid":           {gateway{}, `{"status":"PAID","payment":{"status":"PAID","ref":"pay_C2"}}`},
		"payment failed": {gateway{err: app.ErrPaymentUnavailable}, `{"status":"APPROVED","payment":{"status":"FAILED","ref":null}}`},
		"payments off":   {nil, `{"status":"APPROVED","payment":null}`},
	} {
		st := memory.New()
		memory.Seed(st)
		r := do(t, serverWith(t, st, tc.gateway), staff, `mutation { approveClaim(id: "C2", reason: "ok") { ... on Claim { status payment { status ref } } } }`)
		if got := string(r.Data["approveClaim"]); len(r.Errors) > 0 || got != tc.want {
			t.Errorf("%s: approveClaim = %s %+v, want %s", name, got, r.Errors, tc.want)
		}
	}
}

// busyStore plays a database whose budget lock cannot be had in time.
type busyStore struct{ *memory.Store }

func (busyStore) InsertClaimWithinBudget(context.Context, domain.Claim, app.BudgetCheck) error {
	return fmt.Errorf("insert claim: %w", app.ErrUnavailable)
}

// A transient storage failure is "try again" (UNAVAILABLE), not INTERNAL,
// and it does not leak its cause.
func TestTransientStorageFailureIsUnavailable(t *testing.T) {
	st := memory.New()
	memory.Seed(st)
	r := do(t, serverOn(t, busyStore{st}), provider,
		`mutation { submitClaim(input: {planId: "PL1", category: DAILY_LIVING, amount: "1.00", serviceDate: "2026-05-01"}) { __typename } }`)
	if code(r) != "UNAVAILABLE" || r.Errors[0].Message != "temporarily unavailable, try again" {
		t.Fatalf("want UNAVAILABLE with a retry hint, got %+v", r.Errors)
	}
}
