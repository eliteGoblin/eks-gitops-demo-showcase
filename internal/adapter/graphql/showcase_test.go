package graphql_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/eliteGoblin/eks-gitops-demo/internal/adapter/memory"
)

func TestNodeAndNodesOverGraphQL(t *testing.T) {
	h := server(t)
	r := do(t, h, provider, `{
		p1: node(id: "P1") { __typename id ... on Party { displayName } ... on Participant { dateOfBirth } }
		p2: node(id: "P2") { id }
		c1: node(id: "C1") { ... on Claim { amount } }
		many: nodes(ids: ["PR1", "nope", "PL1"]) { __typename id }
	}`)
	if len(r.Errors) > 0 {
		t.Fatalf("errors: %+v", r.Errors)
	}
	for field, want := range map[string]string{
		"p1":   `{"__typename":"Participant","id":"P1","displayName":"Zhaolin Sun","dateOfBirth":null}`,
		"p2":   `null`,
		"c1":   `{"amount":"250.00"}`,
		"many": `[{"__typename":"Provider","id":"PR1"},null,{"__typename":"Plan","id":"PL1"}]`,
	} {
		if got := string(r.Data[field]); got != want {
			t.Errorf("%s = %s, want %s", field, got, want)
		}
	}
	r = do(t, h, participant, `{ node(id: "P2") { id } participant(id: "P2") { id } }`)
	if string(r.Data["node"]) != "null" || string(r.Data["participant"]) != "null" {
		t.Fatalf("node must hide what participant(id) hides: %v %+v", r.Data, r.Errors)
	}
	ids := `"` + strings.Repeat(`P1","`, 50) + `P1"`
	if r := do(t, h, participant, fmt.Sprintf(`{ nodes(ids: [%s]) { id } }`, ids)); code(r) != "BAD_USER_INPUT" {
		t.Fatalf("51 ids: %+v", r.Errors)
	}
}

func TestSearchOverGraphQL(t *testing.T) {
	h := server(t)
	r := do(t, h, staff, `{ search(term: "sun") { __typename ... on Party { id displayName } ... on Claim { id } } }`)
	want := `[{"__typename":"Participant","id":"P1","displayName":"Zhaolin Sun"},{"__typename":"Provider","id":"PR1","displayName":"Sunrise Care"}]`
	if len(r.Errors) > 0 || string(r.Data["search"]) != want {
		t.Fatalf("search = %s %+v, want %s", r.Data["search"], r.Errors, want)
	}
	for _, q := range []string{`{ search(term: "s") { __typename } }`, `{ search(term: "sun", first: 51) { __typename } }`} {
		if r := do(t, h, staff, q); code(r) != "BAD_USER_INPUT" {
			t.Errorf("%s: %+v", q, r.Errors)
		}
	}
}

func TestBusinessErrorsShareTheUserErrorInterface(t *testing.T) {
	h := server(t)
	for amount, want := range map[string]string{
		"0":        `{"__typename":"ValidationError","message":"amount must be greater than zero","path":["amount"]}`,
		"99999.00": `{"__typename":"BudgetExceededError","message":"budget exceeded: requested 99999.00, remaining`,
	} {
		r := do(t, h, provider, fmt.Sprintf(`mutation { submitClaim(input: {planId: "PL1", category: DAILY_LIVING, amount: %q, serviceDate: "2026-04-20"}) {
			__typename ... on UserError { message path } } }`, amount))
		if len(r.Errors) > 0 || !strings.HasPrefix(string(r.Data["submitClaim"]), want) {
			t.Errorf("amount %s: %s %+v, want prefix %s", amount, r.Data["submitClaim"], r.Errors, want)
		}
	}
}

func TestSubmitClaimIsIdempotentWithAKey(t *testing.T) {
	h := server(t)
	submit := func(amount string) json.RawMessage {
		t.Helper()
		r := do(t, h, provider, fmt.Sprintf(`mutation { submitClaim(input: {planId: "PL1", category: DAILY_LIVING, amount: %q,
			serviceDate: "2026-04-20", idempotencyKey: "gql-retry-1"}) { __typename ... on Claim { id } ... on UserError { message path } } }`, amount))
		if len(r.Errors) > 0 {
			t.Fatalf("errors: %+v", r.Errors)
		}
		return r.Data["submitClaim"]
	}
	first, again := submit("12.00"), submit("12.00")
	if !bytes.Contains(first, []byte(`"__typename":"Claim"`)) || !bytes.Equal(first, again) {
		t.Fatalf("retry must return the first claim: %s vs %s", first, again)
	}
	reused := submit("13.00")
	want := `{"__typename":"IdempotencyKeyReusedError","message":"idempotency key already used for a different request","path":["idempotencyKey"]}`
	if string(reused) != want {
		t.Fatalf("reused key = %s, want %s", reused, want)
	}
}

func TestDeprecatedFieldKeepsWorkingWhileMarked(t *testing.T) {
	h := server(t)
	r := do(t, h, participant, `{ participant(id: "P1") { memberNumber schemeNumber } }`)
	if string(r.Data["participant"]) != `{"memberNumber":"SN100001","schemeNumber":"SN100001"}` {
		t.Fatalf("participant = %s %+v", r.Data["participant"], r.Errors)
	}
	r = do(t, h, participant, `{ __type(name: "Participant") { fields(includeDeprecated: true) { name isDeprecated deprecationReason } } }`)
	if !bytes.Contains(r.Data["__type"], []byte(`{"name":"schemeNumber","isDeprecated":true,"deprecationReason":"Use memberNumber; removal after 2026-12-31"}`)) {
		t.Fatalf("schemeNumber must be deprecated: %s", r.Data["__type"])
	}
}

func TestClaimsFilterSortAndCursorsOverGraphQL(t *testing.T) {
	h := server(t)
	page := func(pr string, after string) (ids []string, end string, errCode string) {
		t.Helper()
		args := `first: 2, orderBy: {field: AMOUNT, direction: DESC}`
		if pr == "default-order" {
			args = `first: 2`
		}
		if after != "" {
			args += fmt.Sprintf(`, after: %q`, after)
		}
		r := do(t, h, staff, fmt.Sprintf(`{ claims(%s) { pageInfo { endCursor } edges { node { id } } } }`, args))
		var conn struct {
			PageInfo struct{ EndCursor string }           `json:"pageInfo"`
			Edges    []struct{ Node struct{ ID string } } `json:"edges"`
		}
		_ = json.Unmarshal(r.Data["claims"], &conn)
		for _, e := range conn.Edges {
			ids = append(ids, e.Node.ID)
		}
		return ids, conn.PageInfo.EndCursor, code(r)
	}
	ids, cursor, _ := page("amount-desc", "")
	if fmt.Sprint(ids) != "[C3 C1]" {
		t.Fatalf("first page by amount desc: %v", ids)
	}
	if ids, _, _ := page("amount-desc", cursor); fmt.Sprint(ids) != "[C2]" {
		t.Fatalf("second page by amount desc: %v", ids)
	}
	if _, _, c := page("default-order", cursor); c != "BAD_USER_INPUT" {
		t.Fatalf("an amount cursor with the default order must be BAD_USER_INPUT, got %q", c)
	}
	for _, q := range []string{
		`{ claims(status: APPROVED, filter: {status: SUBMITTED}) { totalCount } }`,
		`{ claims(filter: {submittedAfter: "yesterday"}) { totalCount } }`,
	} {
		if r := do(t, h, staff, q); code(r) != "BAD_USER_INPUT" {
			t.Errorf("%s: %+v", q, r.Errors)
		}
	}
	r := do(t, h, staff, `{ claims(filter: {providerId: "PR2"}) { edges { node { id } } }
		plan(id: "PL1") { claims(filter: {status: SUBMITTED, submittedAfter: "2026-03-01T00:00:00Z"}) { edges { node { id } } } } }`)
	if len(r.Errors) > 0 || !bytes.Contains(r.Data["claims"], []byte(`"id":"C3"`)) || bytes.Contains(r.Data["claims"], []byte(`"C1"`)) ||
		!bytes.Contains(r.Data["plan"], []byte(`"id":"C2"`)) || bytes.Contains(r.Data["plan"], []byte(`"C1"`)) {
		t.Fatalf("filters: %v %+v", r.Data, r.Errors)
	}
}

// nodes(ids) costs one batch read per record type, however many ids it is
// given: never one probe per id per type.
func TestNodesReadEachTypeOnce(t *testing.T) {
	st := &countingStore{Store: memory.New()}
	memory.Seed(st.Store)
	h := serverOn(t, st)
	pool := []string{"C1", "C2", "C3", "PL1", "PL2", "PL3", "P1", "P2", "P3", "PR1", "PR2", "nope"}
	ids := make([]string, 50)
	for i := range ids {
		ids[i] = pool[i%len(pool)]
	}
	r := do(t, h, staff, fmt.Sprintf(`{ nodes(ids: ["%s"]) { __typename id } }`, strings.Join(ids, `","`)))
	if len(r.Errors) > 0 {
		t.Fatalf("errors: %+v", r.Errors)
	}
	var got []*struct{ ID string }
	if err := json.Unmarshal(r.Data["nodes"], &got); err != nil || len(got) != 50 {
		t.Fatalf("nodes: %s %v", r.Data["nodes"], err)
	}
	for i, id := range ids {
		switch {
		case id == "nope" && got[i] != nil:
			t.Errorf("nodes[%d] (%s) = %+v, want null", i, id, got[i])
		case id != "nope" && (got[i] == nil || got[i].ID != id):
			t.Errorf("nodes[%d] = %+v, want %s", i, got[i], id)
		}
	}
	reads := map[string]int64{
		"GetClaim": st.getClaim.Load(), "GetSupportRequest": st.getSupportRequest.Load(), "GetPlan": st.getPlan.Load(),
		"GetParticipant": st.getParticipant.Load(), "GetProvider": st.getProvider.Load(),
		"GetClaims": st.getClaims.Load(), "GetSupportRequests": st.getSupportRequests.Load(), "GetPlans": st.getPlans.Load(),
		"GetParticipants": st.getParticipants.Load(), "GetProviders": st.getProviders.Load(),
	}
	// GetPlans twice: the probe for plan ids and the plans of the claims found.
	limits := map[string]int64{"GetClaim": 0, "GetSupportRequest": 0, "GetPlan": 0, "GetParticipant": 0, "GetProvider": 0,
		"GetClaims": 1, "GetSupportRequests": 1, "GetPlans": 2, "GetParticipants": 1, "GetProviders": 1}
	for name, max := range limits {
		if reads[name] > max {
			t.Errorf("%s called %d times, at most %d (all reads: %v)", name, reads[name], max, reads)
		}
	}
}

// Lookups by id and searches cost what they do: a fixed base for the probes
// or the scoped queries, plus each item asked for, so aliases cannot pack
// thousands of reads into one cheap-looking request.
func TestNodeAndSearchCostBoundTheRequest(t *testing.T) {
	h := server(t)
	fifty := `"` + strings.Repeat(`P1","`, 49) + `P1"`
	aliases := func(n int, field string) string {
		var b strings.Builder
		b.WriteString("{")
		for i := range n {
			fmt.Fprintf(&b, " a%d: %s", i, field)
		}
		b.WriteString(" }")
		return b.String()
	}
	tests := []struct {
		name, q, code string
	}{
		{"one nodes(50)", fmt.Sprintf(`{ nodes(ids: [%s]) { id } }`, fifty), ""},
		{"five aliased nodes(50)", aliases(5, fmt.Sprintf(`nodes(ids: [%s]) { id }`, fifty)), "COMPLEXITY_LIMIT_EXCEEDED"},
		{"thirty aliased node(id)", aliases(30, `node(id: "P1") { id }`), "COMPLEXITY_LIMIT_EXCEEDED"},
		{"a few searches", aliases(5, `search(term: "sun", first: 1) { __typename }`), ""},
		{"fifteen aliased searches", aliases(15, `search(term: "sun", first: 1) { __typename }`), "COMPLEXITY_LIMIT_EXCEEDED"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if r := do(t, h, staff, tc.q); code(r) != tc.code {
				t.Fatalf("want code %q, got errors %+v", tc.code, r.Errors)
			}
		})
	}
}
