package httpmw

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/eliteGoblin/eks-gitops-demo/internal/platform"
)

// KeyFunc picks the client address a request counts against (its rate-limit
// bucket, and the access log's client_ip).
type KeyFunc func(r *http.Request) string

// RemoteAddr keys by the TCP peer address; use it when no proxy sits in front.
func RemoteAddr(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ForwardedFor keys by the client address behind exactly one trusted proxy
// (the ALB): the right-most X-Forwarded-For entry, which the proxy appends
// itself. Anything left of it is client-controlled and ignored.
func ForwardedFor(r *http.Request) string {
	xff := r.Header.Get("X-Forwarded-For")
	if xff == "" {
		return RemoteAddr(r)
	}
	parts := strings.Split(xff, ",")
	return strings.TrimSpace(parts[len(parts)-1])
}

const cfConnectingIPHeader = "CF-Connecting-IP"

// CFConnectingIP keys by the client address that Cloudflare's edge sets in
// CF-Connecting-IP; cloudflared passes the header through unchanged. Use it
// only where cloudflared is the only possible peer (the ALB retired and a
// NetworkPolicy that admits only the cloudflared namespace): any other peer
// can set the header to anything. Anything but exactly one header line holding
// exactly one IP address keys by the TCP peer instead.
func CFConnectingIP(r *http.Request) string {
	values := r.Header.Values(cfConnectingIPHeader)
	if len(values) != 1 {
		return RemoteAddr(r)
	}
	addr, err := netip.ParseAddr(strings.TrimSpace(values[0]))
	if err != nil || addr.Zone() != "" {
		return RemoteAddr(r)
	}
	return addr.Unmap().String()
}

// RateLimiter is a token bucket per key, with idle buckets swept periodically.
type RateLimiter struct {
	mu        sync.Mutex
	buckets   map[string]*bucket
	limit     rate.Limit
	burst     int
	idle      time.Duration
	lastSweep time.Time
	now       func() time.Time
}

type bucket struct {
	lim  *rate.Limiter
	seen time.Time
}

// NewRateLimiter allows rps requests per second per key with the given burst.
func NewRateLimiter(rps float64, burst int) *RateLimiter {
	return &RateLimiter{buckets: map[string]*bucket{}, limit: rate.Limit(rps), burst: burst, idle: 5 * time.Minute, now: time.Now}
}

// Reserve takes a token for key; when none is left it returns false and how long to wait.
func (l *RateLimiter) Reserve(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if now.Sub(l.lastSweep) > l.idle {
		for k, b := range l.buckets {
			if now.Sub(b.seen) > l.idle {
				delete(l.buckets, k)
			}
		}
		l.lastSweep = now
	}
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{lim: rate.NewLimiter(l.limit, l.burst)}
		l.buckets[key] = b
	}
	b.seen = now
	res := b.lim.ReserveN(now, 1)
	if delay := res.DelayFrom(now); delay > 0 {
		res.CancelAt(now)
		return false, delay
	}
	return true, 0
}

// RateLimit answers 429 with Retry-After once a key runs out of tokens.
func RateLimit(l *RateLimiter, key KeyFunc) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if ok, wait := l.Reserve(key(r)); !ok {
				w.Header().Set("Retry-After", retryAfter(wait))
				platform.WriteError(w, r, http.StatusTooManyRequests, platform.CodeRateLimited, "rate limit exceeded")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
