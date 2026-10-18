package httpmw

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/eliteGoblin/eks-gitops-demo/internal/platform"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

func errorCode(t *testing.T, body []byte) string {
	t.Helper()
	var env struct {
		Errors []struct {
			Extensions map[string]any `json:"extensions"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &env); err != nil || len(env.Errors) == 0 {
		t.Fatalf("not an error envelope: %s", body)
	}
	code, _ := env.Errors[0].Extensions["code"].(string)
	return code
}

func TestRecoverReturns500(t *testing.T) {
	h := RequestID(Recover(discard)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") })))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusInternalServerError || errorCode(t, rec.Body.Bytes()) != platform.CodeInternal {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "request_id") {
		t.Error("error envelope should carry the request id")
	}
}

func TestRecoverRepanicsAbort(t *testing.T) {
	h := Recover(discard)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) }))
	defer func() {
		if recover() != http.ErrAbortHandler { //nolint:errorlint // identity check
			t.Fatal("ErrAbortHandler must propagate")
		}
	}()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
}

func TestRequestID(t *testing.T) {
	var seen string
	h := RequestID(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { seen = platform.RequestIDFrom(r.Context()) }))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Request-ID", "abc-123")
	h.ServeHTTP(rec, req)
	if seen != "abc-123" || rec.Header().Get("X-Request-ID") != "abc-123" {
		t.Fatalf("incoming id not reused: %q", seen)
	}

	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Request-ID", "bad id\nwith newline")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if seen == "" || strings.ContainsAny(seen, " \n") {
		t.Fatalf("unsafe id must be replaced, got %q", seen)
	}
}

type fakeAuth struct{ ok bool }

func (f fakeAuth) Authenticate(ctx context.Context, raw string) (context.Context, error) {
	if !f.ok || raw != "good" {
		return nil, errors.New("bad token")
	}
	return context.WithValue(ctx, ctxKey{}, "alice"), nil
}

type ctxKey struct{}

func TestAuthenticate(t *testing.T) {
	h := Authenticate(fakeAuth{ok: true}, discard)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(r.Context().Value(ctxKey{}).(string)))
	}))
	tests := []struct {
		name   string
		header string
		status int
	}{
		{"no header", "", http.StatusUnauthorized},
		{"wrong scheme", "Basic abc", http.StatusUnauthorized},
		{"empty bearer", "Bearer  ", http.StatusUnauthorized},
		{"bad token", "Bearer nope", http.StatusUnauthorized},
		{"good token", "Bearer good", http.StatusOK},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/graphql", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			h.ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("status %d, want %d", rec.Code, tc.status)
			}
			if tc.status == http.StatusUnauthorized {
				if errorCode(t, rec.Body.Bytes()) != platform.CodeUnauthenticated || rec.Header().Get("WWW-Authenticate") == "" {
					t.Fatalf("401 must carry code and WWW-Authenticate: %s", rec.Body)
				}
			} else if rec.Body.String() != "alice" {
				t.Fatalf("principal context not passed on: %q", rec.Body)
			}
		})
	}
}

func TestRateLimit429(t *testing.T) {
	l := NewRateLimiter(1, 2)
	frozen := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return frozen }
	h := RateLimit(l, RemoteAddr)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

	codes := []int{}
	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "10.0.0.1:5000"
		h.ServeHTTP(rec, req)
		codes = append(codes, rec.Code)
		if rec.Code == http.StatusTooManyRequests {
			if rec.Header().Get("Retry-After") != "1" || errorCode(t, rec.Body.Bytes()) != platform.CodeRateLimited {
				t.Fatalf("429 must carry Retry-After and RATE_LIMITED: %v %s", rec.Header(), rec.Body)
			}
		}
	}
	if codes[0] != 200 || codes[1] != 200 || codes[2] != 429 {
		t.Fatalf("burst 2 then 429, got %v", codes)
	}

	other := httptest.NewRequest(http.MethodGet, "/", nil)
	other.RemoteAddr = "10.0.0.2:5000"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, other)
	if rec.Code != 200 {
		t.Fatalf("another client must have its own bucket, got %d", rec.Code)
	}

	frozen = frozen.Add(10 * time.Minute) // idle buckets are swept and refilled
	if ok, _ := l.Reserve("10.0.0.1"); !ok || len(l.buckets) != 1 {
		t.Fatalf("sweep failed: ok=%v buckets=%d", ok, len(l.buckets))
	}
}

func TestRateLimitKeys(t *testing.T) {
	behindProxy := httptest.NewRequest(http.MethodGet, "/", nil)
	behindProxy.RemoteAddr = "10.1.1.1:443"
	behindProxy.Header.Set("X-Forwarded-For", "6.6.6.6, 203.0.113.9")
	direct := httptest.NewRequest(http.MethodGet, "/", nil)
	direct.RemoteAddr = "10.1.1.1:443"
	garbage := httptest.NewRequest(http.MethodGet, "/", nil)
	garbage.RemoteAddr = "garbage"
	forgedCF := httptest.NewRequest(http.MethodGet, "/", nil)
	forgedCF.RemoteAddr = "10.1.1.1:443"
	forgedCF.Header.Set("X-Forwarded-For", "203.0.113.9")
	forgedCF.Header.Set("CF-Connecting-IP", "198.51.100.66")
	// Through Cloudflare Tunnel (the ALB fallback window keeps TRUST_PROXY on):
	// Cloudflare's edge appends the address that connected to it to a
	// client-sent X-Forwarded-For, or sets the header to that address (equal to
	// CF-Connecting-IP), and cloudflared forwards the request without touching
	// the header. The TCP peer is a cloudflared pod.
	viaTunnel := httptest.NewRequest(http.MethodGet, "/", nil)
	viaTunnel.RemoteAddr = "10.40.7.21:51432"
	viaTunnel.Header.Set("X-Forwarded-For", "6.6.6.6, 198.51.100.7")
	viaTunnel.Header.Set("CF-Connecting-IP", "198.51.100.7")
	viaTunnelNoXFF := httptest.NewRequest(http.MethodGet, "/", nil)
	viaTunnelNoXFF.RemoteAddr = "10.40.7.21:51432"
	viaTunnelNoXFF.Header.Set("X-Forwarded-For", "198.51.100.7")
	viaTunnelNoXFF.Header.Set("CF-Connecting-IP", "198.51.100.7")

	tests := []struct {
		name string
		key  KeyFunc
		req  *http.Request
		want string
	}{
		{"proxy-appended entry wins over spoofed ones", ForwardedFor, behindProxy, "203.0.113.9"},
		{"no X-Forwarded-For falls back to the peer", ForwardedFor, direct, "10.1.1.1"},
		{"peer address ignores the header", RemoteAddr, behindProxy, "10.1.1.1"},
		{"unparseable peer is used as is", RemoteAddr, garbage, "garbage"},
		{"peer address ignores a forged CF-Connecting-IP", RemoteAddr, forgedCF, "10.1.1.1"},
		{"X-Forwarded-For ignores a forged CF-Connecting-IP", ForwardedFor, forgedCF, "203.0.113.9"},
		{"via cloudflared, the edge-appended entry is the client", ForwardedFor, viaTunnel, "198.51.100.7"},
		{"via cloudflared without a client-sent header", ForwardedFor, viaTunnelNoXFF, "198.51.100.7"},
		{"via cloudflared, CF-Connecting-IP names the same client", CFConnectingIP, viaTunnel, "198.51.100.7"},
	}
	for _, tc := range tests {
		if got := tc.key(tc.req); got != tc.want {
			t.Errorf("%s: got %s, want %s", tc.name, got, tc.want)
		}
	}
}

// CF-Connecting-IP carries exactly one address, set by Cloudflare's edge.
// Anything else keys by the TCP peer, so a malformed value can never open a
// fresh bucket, and one client cannot get two buckets by spelling its address
// differently.
func TestCFConnectingIP(t *testing.T) {
	tests := []struct {
		name, header, want string
	}{
		{"the edge's address wins over the peer and X-Forwarded-For", "198.51.100.7", "198.51.100.7"},
		{"IPv6 is keyed in canonical form", "2001:DB8:0:0:0:0:0:1", "2001:db8::1"},
		{"an IPv4-mapped IPv6 address is the IPv4 client", "::ffff:198.51.100.7", "198.51.100.7"},
		{"surrounding spaces are ignored", " 198.51.100.7 ", "198.51.100.7"},
		{"a missing header falls back to the peer", "", "10.1.1.1"},
		{"garbage falls back to the peer", "not-an-ip", "10.1.1.1"},
		{"a list falls back to the peer", "198.51.100.7, 203.0.113.9", "10.1.1.1"},
		{"an address with a port falls back to the peer", "198.51.100.7:443", "10.1.1.1"},
		{"an out-of-range octet falls back to the peer", "999.1.1.1", "10.1.1.1"},
		{"leading zeros fall back to the peer", "198.051.100.007", "10.1.1.1"},
		{"a bracketed address falls back to the peer", "[2001:db8::1]", "10.1.1.1"},
		{"a trailing comma falls back to the peer", "198.51.100.7,", "10.1.1.1"},
		{"a zoned address falls back to the peer", "fe80::1%eth0", "10.1.1.1"},
		{"a zoned IPv4-mapped address falls back to the peer", "::ffff:198.51.100.7%eth0", "10.1.1.1"},
		{"hex IPv4-mapped IPv6 is the IPv4 client", "::ffff:c633:6407", "198.51.100.7"},
	}
	for _, tc := range tests {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "10.1.1.1:443"
		req.Header.Set("X-Forwarded-For", "6.6.6.6")
		if tc.header != "" {
			req.Header.Set("CF-Connecting-IP", tc.header)
		}
		if got := CFConnectingIP(req); got != tc.want {
			t.Errorf("%s: got %s, want %s", tc.name, got, tc.want)
		}
	}

	// Two header lines are a list too (RFC 9110 5.3), even if each is valid.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.1.1.1:443"
	req.Header.Add("CF-Connecting-IP", "198.51.100.7")
	req.Header.Add("CF-Connecting-IP", "198.51.100.8")
	if got := CFConnectingIP(req); got != "10.1.1.1" {
		t.Errorf("two header lines: got %s, want the peer 10.1.1.1", got)
	}
}

func TestLimitBodyAndTimeout(t *testing.T) {
	var deadline bool
	h := Limit(8, time.Second)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, deadline = r.Context().Deadline()
		if _, err := io.ReadAll(r.Body); err != nil {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
		}
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader("0123456789")))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("declared oversize body: got %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", io.NopCloser(bytes.NewReader([]byte("0123456789"))))
	req.ContentLength = -1 // unknown length: MaxBytesReader must still stop it
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge || !deadline {
		t.Fatalf("streamed oversize body: got %d, deadline %v", rec.Code, deadline)
	}
}

// A WebSocket outlives any request deadline; it bounds itself (message size
// limit, keepalive, token lifetime).
func TestLimitLeavesWebSocketsWithoutADeadline(t *testing.T) {
	var deadline bool
	h := Limit(8, time.Second)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, deadline = r.Context().Deadline()
	}))
	req := httptest.NewRequest(http.MethodGet, "/graphql", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "WebSocket")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if deadline {
		t.Fatal("a WebSocket upgrade must not get the request deadline")
	}
	req = httptest.NewRequest(http.MethodPost, "/graphql", nil)
	req.Header.Set("Upgrade", "websocket") // not a handshake: only GET upgrades
	h.ServeHTTP(httptest.NewRecorder(), req)
	if !deadline {
		t.Fatal("a POST must keep its deadline")
	}
}

func TestSecurityHeadersAndCORS(t *testing.T) {
	h := SecurityHeaders(CORS([]string{"https://app.example"})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})))

	req := httptest.NewRequest(http.MethodOptions, "/graphql", nil)
	req.Header.Set("Origin", "https://app.example")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent || rec.Header().Get("Access-Control-Allow-Origin") != "https://app.example" {
		t.Fatalf("preflight from allowed origin: %d %v", rec.Code, rec.Header())
	}
	if rec.Header().Get("X-Frame-Options") != "DENY" || rec.Header().Get("Strict-Transport-Security") == "" {
		t.Fatalf("security headers missing: %v", rec.Header())
	}
	for header, want := range map[string]string{
		"Content-Security-Policy":    "default-src 'none'; frame-ancestors 'none'; base-uri 'none'",
		"Cross-Origin-Opener-Policy": "same-origin",
		"Referrer-Policy":            "no-referrer",
		"Permissions-Policy":         "camera=(), microphone=(), geolocation=(), payment=(), usb=()",
	} {
		if got := rec.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}

	req = httptest.NewRequest(http.MethodPost, "/graphql", nil)
	req.Header.Set("Origin", "https://evil.example")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("disallowed origin must not get CORS headers")
	}
	if rec.Header().Get("Vary") != "Origin" {
		t.Fatal("any response that depends on Origin must carry Vary: Origin, or a cache may replay it to an allowed origin")
	}

	req = httptest.NewRequest(http.MethodPost, "/graphql", nil)
	req.Header.Set("Origin", "https://app.example")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Header().Get("Access-Control-Allow-Origin") != "https://app.example" {
		t.Fatal("allowed origin on simple request must get CORS header")
	}
}

func TestAccessLog(t *testing.T) {
	var buf bytes.Buffer
	log := platform.NewLogger(&buf, slog.LevelInfo)
	clientIP := func(*http.Request) string { return "198.51.100.7" } // what the rate limiter keys on
	h := RequestID(AccessLog(log, clientIP)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("hi"))
	})))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
	line := buf.String()
	if !strings.Contains(line, `"status":418`) || !strings.Contains(line, `"bytes":2`) || !strings.Contains(line, `"request_id"`) {
		t.Fatalf("access log line: %s", line)
	}
	if !strings.Contains(line, `"client_ip":"198.51.100.7"`) || !strings.Contains(line, `"remote":"192.0.2.1:1234"`) {
		t.Fatalf("access log must carry the client IP next to the TCP peer: %s", line)
	}
}

func TestErrorFormatAppliesToRecover(t *testing.T) {
	h := ErrorFormat(platform.RESTErrors)(Recover(slog.New(slog.DiscardHandler))(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	})))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), `"error":{`) {
		t.Fatalf("status %d body %s", rec.Code, rec.Body)
	}
}

func TestAccessLogUsesTheRouteTemplate(t *testing.T) {
	var buf bytes.Buffer
	r := chi.NewRouter()
	r.Use(AccessLog(platform.NewLogger(&buf, slog.LevelInfo), RemoteAddr))
	r.Get("/payments/{id}", func(http.ResponseWriter, *http.Request) {})
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/payments/pay_7XQ2", nil))
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/nope/123", nil))
	logs := buf.String()
	// The route label is bounded (one value per route) so metrics built on it
	// cannot explode in cardinality; the raw path stays only in the log line.
	if !strings.Contains(logs, `"route":"/payments/{id}"`) || !strings.Contains(logs, `"route":"unmatched"`) {
		t.Fatalf("access log lines: %s", logs)
	}
}

// A request that panics still gets its access-log line, with the 500 the
// client receives: Recover sits outside AccessLog.
func TestAccessLogRecordsAPanickingRequestAs500(t *testing.T) {
	var buf bytes.Buffer
	log := platform.NewLogger(&buf, slog.LevelInfo)
	h := Recover(discard)(AccessLog(log, RemoteAddr)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	})))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", rec.Code)
	}
	if line := buf.String(); !strings.Contains(line, `"msg":"http"`) || !strings.Contains(line, `"status":500`) {
		t.Fatalf("access log for a panicking request: %q", line)
	}
}

// Only the subscription endpoint's handshake is exempt from the request
// deadline; an Upgrade header on any other path exempts nothing.
func TestOnlyTheSubscriptionHandshakeEscapesTheDeadline(t *testing.T) {
	var deadline bool
	h := Limit(8, time.Second)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, deadline = r.Context().Deadline()
	}))
	req := httptest.NewRequest(http.MethodGet, "/payments", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if !deadline {
		t.Fatal("an Upgrade header on another path must not lift the deadline")
	}
	if IsWebSocketUpgrade(req) {
		t.Fatal("IsWebSocketUpgrade must require the subscription path")
	}
}
