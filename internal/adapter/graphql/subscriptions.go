package graphql

import (
	"bufio"
	"cmp"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	gql "github.com/99designs/gqlgen/graphql"
	"github.com/99designs/gqlgen/graphql/handler/transport"
	coderws "github.com/coder/websocket"
	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/gqlerror"

	"github.com/eliteGoblin/eks-gitops-demo/internal/app"
	"github.com/eliteGoblin/eks-gitops-demo/internal/platform"
	"github.com/eliteGoblin/eks-gitops-demo/internal/platform/authn"
)

// Authenticator verifies a bearer token and returns a context carrying the
// caller's principal; the OIDC adapter implements it.
type Authenticator interface {
	Authenticate(ctx context.Context, rawToken string) (context.Context, error)
}

// SubscriptionOptions turn on live updates over WebSocket (graphql-transport-ws).
// The upgrade request carries no token: the client authenticates in
// connection_init, and the connection ends when that token expires.
type SubscriptionOptions struct {
	Authenticator    Authenticator
	AllowedOrigins   []string      // cross-origin pages that may connect, exact origins as for CORS
	MaxPerConnection int           // concurrent subscriptions on one connection; 0 means 5
	PingInterval     time.Duration // keepalive under the load balancer's idle timeout; 0 means 25s
	InitTimeout      time.Duration // how long a client may take to authenticate; 0 means 10s
	// CloseGrace is how long a socket may stay open after the protocol is
	// done with it, so a close handshake can finish; then it is closed
	// regardless. 0 means 6s (the WebSocket library waits up to 5s).
	CloseGrace time.Duration
}

// graphql-transport-ws close codes.
const (
	closeUnauthorized = 4401
	closeForbidden    = 4403
)

const (
	defaultMaxPerConnection = 5
	defaultPingInterval     = 25 * time.Second // the ALB closes connections idle for 60s
	defaultInitTimeout      = 10 * time.Second
	defaultCloseGrace       = 6 * time.Second
	maxMessageBytes         = 64 << 10
)

// Each call returns a fresh error: gqlgen annotates errors in place.
func errTooManySubscriptions() *gqlerror.Error {
	return coded(platform.CodeRateLimited, "too many subscriptions on this connection")
}

func errNeedsWebsocket() *gqlerror.Error {
	return coded(platform.CodeBadUserInput, "subscriptions need a WebSocket connection (graphql-transport-ws)")
}

func errSubscriptionsOnly() *gqlerror.Error {
	return coded(platform.CodeBadUserInput, "only subscriptions are served over WebSocket; send queries and mutations with POST")
}

func coded(code, msg string) *gqlerror.Error {
	return &gqlerror.Error{Message: msg, Extensions: map[string]any{"code": code}}
}

// connection is the state of one authenticated WebSocket, kept in its context.
type connection struct {
	slots  chan struct{}      // one per open subscription
	cancel context.CancelFunc // ends the connection when the token expires
}

type connectionKey struct{}

func connectionFrom(ctx context.Context) (*connection, bool) {
	c, ok := ctx.Value(connectionKey{}).(*connection)
	return c, ok
}

// websocketTransport authenticates connection_init, keeps the connection
// alive and refuses cross-origin pages that are not allowed.
func websocketTransport(opt SubscriptionOptions, log *slog.Logger) gql.Transport {
	maxPer := cmp.Or(opt.MaxPerConnection, defaultMaxPerConnection)
	ping := cmp.Or(opt.PingInterval, defaultPingInterval)
	readLimit := int64(maxMessageBytes)
	return originCheck{
		allowed:    opt.AllowedOrigins,
		closeGrace: cmp.Or(opt.CloseGrace, defaultCloseGrace),
		Websocket: transport.Websocket{
			Implementation: transport.CoderWebsocketImplementation{
				AcceptOptions: coderws.AcceptOptions{OriginPatterns: originHosts(opt.AllowedOrigins)},
			},
			InitFunc:              authenticateConnection(opt.Authenticator, maxPer, log),
			InitTimeout:           cmp.Or(opt.InitTimeout, defaultInitTimeout),
			CloseFunc:             func(ctx context.Context, _ int) { endConnection(ctx) },
			ErrorFunc:             func(ctx context.Context, err error) { log.InfoContext(ctx, "websocket error", "err", err) },
			KeepAlivePingInterval: ping, // legacy graphql-ws clients
			PingPongInterval:      ping, // graphql-transport-ws clients; a peer that stops answering is dropped
			PayloadReadLimit:      &readLimit,
		},
	}
}

// authenticateConnection verifies the bearer token in connection_init and
// bounds the connection by the token's lifetime.
func authenticateConnection(auth Authenticator, maxPer int, log *slog.Logger) transport.WebsocketInitFunc {
	return func(ctx context.Context, payload transport.InitPayload) (context.Context, *transport.InitPayload, error) {
		raw, ok := strings.CutPrefix(payload.Authorization(), "Bearer ")
		if !ok || strings.TrimSpace(raw) == "" {
			return refuse(ctx, closeUnauthorized, "Unauthorized")
		}
		authed, err := auth.Authenticate(ctx, strings.TrimSpace(raw))
		if err != nil {
			log.InfoContext(ctx, "websocket token rejected", "err", err)
			if errors.Is(err, authn.ErrClientNotAllowed) {
				return refuse(ctx, closeForbidden, "Forbidden")
			}
			return refuse(ctx, closeUnauthorized, "Unauthorized")
		}
		conn := &connection{slots: make(chan struct{}, maxPer)}
		authed = context.WithValue(authed, connectionKey{}, conn)
		if pr, ok := app.PrincipalFrom(authed); ok && !pr.ExpiresAt.IsZero() {
			authed, conn.cancel = context.WithDeadline(authed, pr.ExpiresAt)
		}
		return authed, nil, nil
	}
}

// refuse makes gqlgen close the socket with code and reason.
func refuse(ctx context.Context, code int, reason string) (context.Context, *transport.InitPayload, error) {
	ctx = transport.AppendCloseReason(transport.WithWebsocketCloseCode(ctx, code), reason)
	return ctx, nil, errors.New(reason)
}

func endConnection(ctx context.Context) {
	if c, ok := connectionFrom(ctx); ok && c.cancel != nil {
		c.cancel()
	}
}

// subscribe opens a watch within the connection's subscription budget and
// converts each update for the GraphQL layer. The slot is free again before
// the returned channel closes.
func subscribe[T, M any](ctx context.Context, planID string, watch func(context.Context, string) (<-chan T, error), wrap func(T) *M) (<-chan *M, error) {
	conn, ok := connectionFrom(ctx)
	if !ok {
		return nil, errNeedsWebsocket()
	}
	select {
	case conn.slots <- struct{}{}:
	default:
		return nil, errTooManySubscriptions()
	}
	release := func() { <-conn.slots }
	updates, err := watch(ctx, planID)
	if err != nil {
		release()
		return nil, err
	}
	out := make(chan *M)
	go func() {
		defer close(out)
		defer release()
		for u := range updates {
			select {
			case out <- wrap(u):
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

// operationsByTransport keeps each transport to its job: subscriptions only
// over WebSocket, where the HTTP request limits (rate, timeout) do not apply,
// and queries and mutations only over POST.
func operationsByTransport(ctx context.Context, next gql.OperationHandler) gql.ResponseHandler {
	_, overWebsocket := connectionFrom(ctx)
	isSubscription := gql.GetOperationContext(ctx).Operation.Operation == ast.Subscription
	switch {
	case overWebsocket && !isSubscription:
		return gql.OneShot(&gql.Response{Errors: gqlerror.List{errSubscriptionsOnly()}})
	case !overWebsocket && isSubscription:
		return gql.OneShot(&gql.Response{Errors: gqlerror.List{errNeedsWebsocket()}})
	}
	return next(ctx)
}

// originCheck refuses WebSocket upgrades from pages on other origins, which
// would otherwise ride on a user's open session (cross-site WebSocket
// hijacking). Requests without an Origin are not from a browser page.
//
// It also closes every socket once gqlgen is done with it, after closeGrace
// (time for a close handshake): gqlgen returns without closing the socket
// when connection_init's payload is not an object, which would otherwise
// leave unauthenticated sockets open indefinitely.
type originCheck struct {
	transport.Websocket
	allowed    []string
	closeGrace time.Duration
}

func (t originCheck) Do(w http.ResponseWriter, r *http.Request, exec gql.GraphExecutor) {
	if origin := r.Header.Get("Origin"); origin != "" && !sameHost(origin, r.Host) && !slices.Contains(t.allowed, origin) {
		platform.WriteError(w, r, http.StatusForbidden, platform.CodeForbidden, "origin not allowed")
		return
	}
	hw := &hijackRecorder{ResponseWriter: w}
	defer hw.closeAfter(t.closeGrace) // deferred: runs even if the transport panics
	t.Websocket.Do(hw, r, exec)
}

// hijackRecorder remembers the connection the WebSocket library takes over,
// so the transport can make sure it is closed.
type hijackRecorder struct {
	http.ResponseWriter
	conn net.Conn
}

func (h *hijackRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := http.NewResponseController(h.ResponseWriter).Hijack()
	if err == nil {
		h.conn = conn
	}
	return conn, rw, err
}

func (h *hijackRecorder) Unwrap() http.ResponseWriter { return h.ResponseWriter }

// closeAfter closes the hijacked connection, if there is one, after grace
// (a no-op if it is already closed).
func (h *hijackRecorder) closeAfter(grace time.Duration) {
	if h.conn != nil {
		time.AfterFunc(grace, func() { _ = h.conn.Close() })
	}
}

func sameHost(origin, host string) bool {
	u, err := url.Parse(origin)
	return err == nil && strings.EqualFold(u.Host, host)
}

// originHosts turns allowed origins into the host patterns the WebSocket
// library checks again on accept.
func originHosts(origins []string) []string {
	hosts := make([]string, 0, len(origins))
	for _, o := range origins {
		if u, err := url.Parse(o); err == nil && u.Host != "" {
			hosts = append(hosts, u.Host)
		}
	}
	return hosts
}
