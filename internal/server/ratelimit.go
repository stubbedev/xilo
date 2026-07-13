package server

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// loginLimiter throttles password attempts per client IP. bcrypt costs
// ~100ms of CPU per attempt, so an unthrottled /admin/login is a free
// CPU-exhaustion vector (and a brute-force one). Token bucket: burst of 5,
// one token back every 10s, entries pruned lazily.
type loginLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	now     func() time.Time // test seam
}

type bucket struct {
	tokens float64
	last   time.Time
}

const (
	loginBurst  = 10
	loginRefill = 10 * time.Second // one attempt back per this interval
)

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{buckets: map[string]*bucket{}, now: time.Now}
}

// allow reports whether ip may attempt a login now, consuming a token if so.
func (l *loginLimiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()

	// Lazy prune: full buckets carry no state worth keeping.
	if len(l.buckets) > 4096 {
		for k, b := range l.buckets {
			if now.Sub(b.last) > loginBurst*loginRefill {
				delete(l.buckets, k)
			}
		}
	}

	b := l.buckets[ip]
	if b == nil {
		b = &bucket{tokens: loginBurst, last: now}
		l.buckets[ip] = b
	}
	b.tokens += now.Sub(b.last).Seconds() / loginRefill.Seconds()
	if b.tokens > loginBurst {
		b.tokens = loginBurst
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// clientIP extracts the real client IP for rate limiting and activities.
// Forwarding headers are honored only when the direct peer is a loopback or
// private address — i.e. a reverse proxy colocated with xilo, the default
// deployment. A client connecting directly from a public address can't forge
// its IP: its peer is public, so its headers are ignored and rate limiting
// keys on the real socket peer.
//
// The trusted proxy APPENDS the real client as the Nth-from-last entry of
// X-Forwarded-For (nginx's proxy_add_x_forwarded_for and equivalents), so we
// index from the right by TrustedProxyHops. The leftmost entries are
// client-supplied and MUST NOT be trusted — reading them lets an attacker set
// X-Forwarded-For: <random> per request and rotate past the rate limiter.
func (s *Server) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	hops := s.cfg.TrustedProxyHops
	if hops < 0 {
		return host // proxy trust disabled: always key on the socket peer
	}
	if hops == 0 {
		hops = 1 // default: one colocated reverse proxy
	}
	ip := net.ParseIP(host)
	if ip == nil || !(ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()) {
		return host
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		// The entry our own proxy wrote sits hops from the right.
		if idx := len(parts) - hops; idx >= 0 && idx < len(parts) {
			if v := strings.TrimSpace(parts[idx]); v != "" {
				return v
			}
		}
	}
	// Fallback: X-Real-IP, which the trusted proxy must OVERWRITE (not pass
	// through) for this to be safe. Single-valued, so it can't be hop-indexed.
	if xr := strings.TrimSpace(r.Header.Get("X-Real-Ip")); xr != "" {
		return xr
	}
	return host
}
