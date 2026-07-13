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

// clientIP extracts the remote IP. X-Forwarded-For / X-Real-IP are honored
// only when trustProxy is set (security.trusted_proxy): those headers are
// client-forgeable unless a trusted proxy overwrites them, and rate limiting
// on a forgeable key is no rate limiting at all. Without it, all logins behind
// a reverse proxy share the proxy's IP — burst 5/10s still leaves a human plenty.
func clientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if xr := strings.TrimSpace(r.Header.Get("X-Real-Ip")); xr != "" {
			return xr
		}
		// Leftmost entry is the original client; the rest are proxy hops.
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if i := strings.IndexByte(xff, ','); i >= 0 {
				xff = xff[:i]
			}
			if xff = strings.TrimSpace(xff); xff != "" {
				return xff
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
