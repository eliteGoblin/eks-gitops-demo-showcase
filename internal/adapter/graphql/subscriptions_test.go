package graphql_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/eliteGoblin/eks-gitops-demo/internal/adapter/events"
	"github.com/eliteGoblin/eks-gitops-demo/internal/adapter/graphql"
	"github.com/eliteGoblin/eks-gitops-demo/internal/adapter/memory"
	"github.com/eliteGoblin/eks-gitops-demo/internal/app"
	"github.com/eliteGoblin/eks-gitops-demo/internal/domain"
	"github.com/eliteGoblin/eks-gitops-demo/internal/platform/authn"
)

const tokenLifetime = 300 * time.Millisecond

// tokenAuth maps fixed test tokens to principals, as the OIDC adapter maps
// verified access tokens.
type tokenAuth struct{}

func (tokenAuth) Authenticate(ctx context.Context, raw string) (context.Context, error) {
	switch raw {
	case "staff":
		return app.WithPrincipal(ctx, *staff), nil
	case "p1":
		return app.WithPrincipal(ctx, *participant), nil
	case "p3":
		return app.WithPrincipal(ctx, app.Principal{Subject: "p3", Roles: map[app.Role]bool{app.RoleParticipant: true}, ParticipantID: "P3"}), nil
	case "expiring":
		p := *staff
		p.ExpiresAt = time.Now().Add(tokenLifetime)
		return app.WithPrincipal(ctx, p), nil
	case "other-client":
		return nil, fmt.Errorf("%w: %q", authn.ErrClientNotAllowed, "other")
	}
	return nil, errors.New("invalid token")
}

// signallingEvents tells the test when a subscriber has registered, so a
// change made afterwards is certain to reach it.
type signallingEvents struct {
	app.Events
	subscribed chan struct{}
}

func (e signallingEvents) Subscribe(ctx context.Context, kind app.EventKind, planID string) (<-chan app.Event, error) {
	ch, err := e.Events.Subscribe(ctx, kind, planID)
	e.subscribed <- struct{}{}
	return ch, err
}

// countingPlans counts the batched plan reads behind Claim.plan.
type countingPlans struct {
	store
	batches atomic.Int64
}

func (c *countingPlans) GetPlans(ctx context.Context, ids []string) ([]domain.Plan, error) {
	c.batches.Add(1)
	return c.store.GetPlans(ctx, ids)
}

type wsServer struct {
	url        string
	gql        http.Handler
	plans      *countingPlans
	subscribed chan struct{}
}

func newWSServer(t *testing.T, allowedOrigins ...string) wsServer {
	t.Helper()
	return newWSServerWith(t, func(o *graphql.SubscriptionOptions) { o.AllowedOrigins = allowedOrigins })
}

// newWSServerWith is newWSServer with its subscription options adjusted by tweak.
func newWSServerWith(t *testing.T, tweak func(*graphql.SubscriptionOptions)) wsServer {
	t.Helper()
	st := memory.New()
	memory.Seed(st)
	plans := &countingPlans{store: st}
	log := slog.New(slog.DiscardHandler)
	evs := signallingEvents{Events: events.NewHub(log), subscribed: make(chan struct{}, 16)}
	var n atomic.Int64
	svc, err := app.NewServices(app.Deps{Participants: st, Plans: plans, Providers: st, Claims: st, Bookings: st, SupportRequests: st,
		Relationships: st, Auditor: nopAudit{}, Events: evs,
		Clock: func() time.Time { return time.Date(2026, 5, 2, 0, 0, 0, 0, time.UTC) }, NextID: func() string { return fmt.Sprintf("N%d", n.Add(1)) }})
	if err != nil {
		t.Fatal(err)
	}
	opts := graphql.SubscriptionOptions{Authenticator: tokenAuth{}, MaxPerConnection: 2}
	tweak(&opts)
	h := graphql.NewHandler(svc, graphql.Options{DepthLimit: 8, ComplexityLimit: 300, Log: log, Subscriptions: &opts})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return wsServer{url: "ws" + strings.TrimPrefix(srv.URL, "http"), gql: h, plans: plans, subscribed: evs.subscribed}
}

// waitSubscribed waits until n more subscribers have registered.
func (s wsServer) waitSubscribed(t *testing.T, n int) {
	t.Helper()
	for range n {
		select {
		case <-s.subscribed:
		case <-time.After(3 * time.Second):
			t.Fatal("subscription never registered")
		}
	}
}

type wsClient struct {
	t    *testing.T
	conn *websocket.Conn
}

// dial opens a WebSocket, sending origin as the browser would; on failure it
// returns the handshake's HTTP status.
func dial(t *testing.T, s wsServer, origin string) (*wsClient, int, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	h := http.Header{}
	if origin != "" {
		h.Set("Origin", origin)
	}
	//nolint:bodyclose // websocket.Dial: "You never need to close resp.Body yourself"
	conn, resp, err := websocket.Dial(ctx, s.url, &websocket.DialOptions{Subprotocols: []string{"graphql-transport-ws"}, HTTPHeader: h})
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		return nil, status, err
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	return &wsClient{t: t, conn: conn}, http.StatusSwitchingProtocols, nil
}

// connect dials and authenticates with token.
func connect(t *testing.T, s wsServer, token string) *wsClient {
	t.Helper()
	c, _, err := dial(t, s, "")
	if err != nil {
		t.Fatal(err)
	}
	c.send(map[string]any{"type": "connection_init", "payload": map[string]any{"Authorization": "Bearer " + token}})
	c.expect("connection_ack")
	return c
}

func (c *wsClient) send(msg map[string]any) {
	c.t.Helper()
	b, _ := json.Marshal(msg)
	if err := c.conn.Write(context.Background(), websocket.MessageText, b); err != nil {
		c.t.Fatalf("send: %v", err)
	}
}

func (c *wsClient) subscribe(id, query string) {
	c.t.Helper()
	c.send(map[string]any{"id": id, "type": "subscribe", "payload": map[string]any{"query": query}})
}

// read returns the next protocol message, answering pings, or the error that
// ended the connection.
func (c *wsClient) read() (map[string]any, error) {
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_, b, err := c.conn.Read(ctx)
		cancel()
		if err != nil {
			return nil, err
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			c.t.Fatalf("bad message %s", b)
		}
		if m["type"] == "ping" {
			c.send(map[string]any{"type": "pong"})
			continue
		}
		return m, nil
	}
}

func (c *wsClient) expect(typ string) map[string]any {
	c.t.Helper()
	m, err := c.read()
	if err != nil {
		c.t.Fatalf("waiting for %q: %v", typ, err)
	}
	if m["type"] != typ {
		c.t.Fatalf("want %q, got %v", typ, m)
	}
	return m
}

// closeStatus reads until the server closes the connection.
func (c *wsClient) closeStatus() websocket.StatusCode {
	for {
		if _, err := c.read(); err != nil {
			return websocket.CloseStatus(err)
		}
	}
}

func payloadOf(m map[string]any) string {
	b, _ := json.Marshal(m["payload"])
	return string(b)
}

// expectRefused checks that operation id ended with an error coded code.
func (c *wsClient) expectRefused(id, code string) {
	c.t.Helper()
	m := c.expect("next")
	if m["id"] != id || !strings.Contains(payloadOf(m), `"code":"`+code+`"`) {
		c.t.Fatalf("want operation %s refused with %s, got %v", id, code, m)
	}
	if m = c.expect("complete"); m["id"] != id {
		c.t.Fatalf("want operation %s completed, got %v", id, m)
	}
}

func TestConnectionInitNeedsAValidToken(t *testing.T) {
	s := newWSServer(t)
	for name, tc := range map[string]struct {
		payload map[string]any
		want    websocket.StatusCode
	}{
		"no token":           {map[string]any{}, 4401},
		"invalid token":      {map[string]any{"Authorization": "Bearer nope"}, 4401},
		"not a bearer token": {map[string]any{"Authorization": "Basic abc"}, 4401},
		"client not allowed": {map[string]any{"Authorization": "Bearer other-client"}, 4403},
	} {
		c, _, err := dial(t, s, "")
		if err != nil {
			t.Fatal(err)
		}
		c.send(map[string]any{"type": "connection_init", "payload": tc.payload})
		if got := c.closeStatus(); got != tc.want {
			t.Errorf("%s: close status %d, want %d", name, got, tc.want)
		}
	}
}

// A subscriber hears about a change made through another request, re-read
// with its own access; nested fields are loaded afresh for every update.
func TestSubscriptionDeliversChanges(t *testing.T) {
	s := newWSServer(t)
	c := connect(t, s, "p1")
	c.subscribe("1", `subscription { claimStatusChanged(planId: "PL1") { id status payment { status ref } plan { id } } }`)
	s.waitSubscribed(t, 1)

	if r := do(t, s.gql, staff, `mutation { approveClaim(id: "C2", reason: "ok") { __typename } }`); len(r.Errors) > 0 {
		t.Fatalf("approve: %+v", r.Errors)
	}
	m := c.expect("next")
	if got := payloadOf(m); m["id"] != "1" || got != `{"data":{"claimStatusChanged":{"id":"C2","payment":null,"plan":{"id":"PL1"},"status":"APPROVED"}}}` {
		t.Fatalf("update = %s", got)
	}
	if r := do(t, s.gql, provider, `mutation { submitClaim(input: {planId: "PL1", category: DAILY_LIVING, amount: "10.00", serviceDate: "2026-05-01"}) { __typename } }`); len(r.Errors) > 0 {
		t.Fatalf("submit: %+v", r.Errors)
	}
	if got := payloadOf(c.expect("next")); !strings.Contains(got, `"status":"SUBMITTED"`) {
		t.Fatalf("update = %s", got)
	}
	if n := s.plans.batches.Load(); n != 2 {
		t.Fatalf("Claim.plan was batch-loaded %d times over two updates, want 2 (a loader per update)", n)
	}
}

func TestSupportRequestSubscription(t *testing.T) {
	s := newWSServer(t)
	c := connect(t, s, "p1")
	c.subscribe("1", `subscription { supportRequestStatusChanged(planId: "PL1") { id status plan { id } } }`)
	s.waitSubscribed(t, 1)
	r := do(t, s.gql, participant, `mutation { raiseSupportRequest(input: {planId: "PL1", category: THERAPY, description: "Weekly physio", estimatedAmount: "100.00"}) { __typename } }`)
	if len(r.Errors) > 0 {
		t.Fatalf("raise: %+v", r.Errors)
	}
	if got := payloadOf(c.expect("next")); !strings.Contains(got, `"status":"PENDING"`) || !strings.Contains(got, `"plan":{"id":"PL1"}`) {
		t.Fatalf("update = %s", got)
	}
}

func TestSubscribingToAPlanYouCannotReadIsForbidden(t *testing.T) {
	s := newWSServer(t)
	c := connect(t, s, "p3")
	c.subscribe("1", `subscription { claimStatusChanged(planId: "PL1") { id } }`)
	c.expectRefused("1", "FORBIDDEN")
}

func TestSubscriptionsPerConnectionAreCapped(t *testing.T) {
	s := newWSServer(t)
	c := connect(t, s, "staff")
	c.subscribe("1", `subscription { claimStatusChanged(planId: "PL1") { id } }`)
	c.subscribe("2", `subscription { supportRequestStatusChanged(planId: "PL1") { id } }`)
	s.waitSubscribed(t, 2)
	c.subscribe("3", `subscription { claimStatusChanged(planId: "PL2") { id } }`)
	c.expectRefused("3", "RATE_LIMITED")
}

// Queries and mutations go over POST, where the request limits apply;
// subscriptions need the WebSocket.
func TestEachTransportServesItsOperations(t *testing.T) {
	s := newWSServer(t)
	c := connect(t, s, "staff")
	c.subscribe("1", `{ me { subject } }`)
	c.expectRefused("1", "BAD_USER_INPUT")
	if r := do(t, s.gql, staff, `subscription { claimStatusChanged(planId: "PL1") { id } }`); code(r) != "BAD_USER_INPUT" {
		t.Fatalf("a subscription over POST: %+v", r)
	}
}

func TestCrossOriginPagesCannotConnect(t *testing.T) {
	s := newWSServer(t, "https://portal.example")
	if _, status, err := dial(t, s, "https://evil.example"); err == nil || status != http.StatusForbidden {
		t.Fatalf("a foreign origin must get 403, got %d %v", status, err)
	}
	if _, _, err := dial(t, s, "https://portal.example"); err != nil {
		t.Fatalf("an allowed origin must connect: %v", err)
	}
	if _, _, err := dial(t, s, "http://"+strings.TrimPrefix(s.url, "ws://")); err != nil {
		t.Fatalf("the same origin must connect: %v", err)
	}
	if _, _, err := dial(t, s, ""); err != nil {
		t.Fatalf("a client without Origin (not a browser page) must connect: %v", err)
	}
}

// The connection ends, normally, when the token it authenticated with expires.
func TestConnectionEndsWhenTheTokenExpires(t *testing.T) {
	s := newWSServer(t)
	c := connect(t, s, "expiring")
	c.subscribe("1", `subscription { claimStatusChanged(planId: "PL1") { id } }`)
	start := time.Now()
	if got := c.closeStatus(); got != websocket.StatusNormalClosure {
		t.Fatalf("close status %d, want %d", got, websocket.StatusNormalClosure)
	}
	if waited := time.Since(start); waited > tokenLifetime+time.Second {
		t.Fatalf("the connection outlived its token by %v", waited-tokenLifetime)
	}
}

// Without an upgrade, GET executes nothing: the route is outside the HTTP
// authentication, which the WebSocket replaces with connection_init.
func TestPlainGetExecutesNothing(t *testing.T) {
	s := newWSServer(t)
	resp, err := http.Get("http" + strings.TrimPrefix(s.url, "ws") + "?query=%7Bme%7Bsubject%7D%7D")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("GET without upgrade: %d, want 400", resp.StatusCode)
	}
}

// gqlgen leaves the socket open when connection_init carries a payload that
// is not an object: it neither answers nor closes, and its init timeout no
// longer applies. The transport must close such a socket itself, once the
// grace period a close handshake needs has passed.
func TestMalformedConnectionInitIsClosed(t *testing.T) {
	s := newWSServerWith(t, func(o *graphql.SubscriptionOptions) { o.CloseGrace = 200 * time.Millisecond })
	c, _, err := dial(t, s, "")
	if err != nil {
		t.Fatal(err)
	}
	c.send(map[string]any{"type": "connection_init", "payload": 123})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, _, err := c.conn.Read(ctx); errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("the server left the socket open after a malformed connection_init")
	}
}
