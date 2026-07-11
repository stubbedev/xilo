package server

import (
	"net"
	"net/http"
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

// clientIP extracts the remote IP. Deliberately ignores X-Forwarded-For: it
// is client-forgeable unless a trusted proxy strips it, and rate limiting on
// a forgeable key is no rate limiting at all. Behind a reverse proxy all
// logins share the proxy's IP — burst 5/10s still leaves a human plenty.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
