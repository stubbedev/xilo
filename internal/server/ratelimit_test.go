package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stubbedev/xilo/internal/config"
)

func TestLoginLimiterBucket(t *testing.T) {
	now := time.Unix(1000, 0)
	l := newLoginLimiter()
	l.now = func() time.Time { return now }

	for i := 0; i < loginBurst; i++ {
		if !l.allow("1.2.3.4") {
			t.Fatalf("attempt %d within burst denied", i)
		}
	}
	if l.allow("1.2.3.4") {
		t.Fatal("attempt past burst allowed")
	}
	// Other IPs unaffected.
	if !l.allow("5.6.7.8") {
		t.Fatal("independent ip throttled")
	}
	// One refill interval → exactly one more attempt.
	now = now.Add(loginRefill)
	if !l.allow("1.2.3.4") {
		t.Fatal("refilled token denied")
	}
	if l.allow("1.2.3.4") {
		t.Fatal("second attempt after single refill allowed")
	}
	// Long idle → back to full burst, capped.
	now = now.Add(time.Hour)
	for i := 0; i < loginBurst; i++ {
		if !l.allow("1.2.3.4") {
			t.Fatalf("post-idle attempt %d denied", i)
		}
	}
	if l.allow("1.2.3.4") {
		t.Fatal("burst not capped after idle")
	}
}

func TestLoginLimiterPrune(t *testing.T) {
	now := time.Unix(1000, 0)
	l := newLoginLimiter()
	l.now = func() time.Time { return now }
	for i := 0; i < 5000; i++ {
		l.allow(string(rune(i)) + ".ip")
	}
	now = now.Add(24 * time.Hour)
	l.allow("fresh") // triggers prune
	if len(l.buckets) > 10 {
		t.Fatalf("prune left %d buckets", len(l.buckets))
	}
}

func TestClientIP(t *testing.T) {
	s := &Server{cfg: &config.Config{}} // TrustedProxyHops 0 → default 1
	r := httptest.NewRequest(http.MethodPost, "/admin/login", nil)
	r.RemoteAddr = "10.1.2.3:5555"
	if got := s.clientIP(r); got != "10.1.2.3" {
		t.Fatalf("clientIP = %q", got)
	}
	r.RemoteAddr = "weird"
	if got := s.clientIP(r); got != "weird" {
		t.Fatalf("clientIP fallback = %q", got)
	}
	// Direct connection from a PUBLIC peer: forwarding headers are forgeable
	// and must be ignored — the socket peer wins.
	r.Header.Set("X-Forwarded-For", "6.6.6.6")
	r.Header.Set("X-Real-Ip", "7.7.7.7")
	r.RemoteAddr = "203.0.113.9:5555"
	if got := s.clientIP(r); got != "203.0.113.9" {
		t.Fatalf("public peer honored forgeable header: %q", got)
	}
	// Behind a proxy on a private peer: a client-forged leftmost XFF entry must
	// be ignored. With one trusted hop the RIGHTMOST entry (what the proxy
	// appended) is the real client — the forged 6.6.6.6 must not win.
	r.Header.Del("X-Real-Ip")
	r.Header.Set("X-Forwarded-For", "6.6.6.6, 198.51.100.7")
	r.RemoteAddr = "10.0.0.9:5555"
	if got := s.clientIP(r); got != "198.51.100.7" {
		t.Fatalf("proxy XFF took forgeable leftmost: %q", got)
	}
	// Two trusted hops: index two from the right, still skipping the forgery.
	s.cfg.TrustedProxyHops = 2
	r.Header.Set("X-Forwarded-For", "6.6.6.6, 198.51.100.7, 10.0.0.2")
	if got := s.clientIP(r); got != "198.51.100.7" {
		t.Fatalf("two-hop XFF = %q", got)
	}
	// X-Real-IP is only a fallback when no XFF is present.
	s.cfg.TrustedProxyHops = 0
	r.Header.Del("X-Forwarded-For")
	r.Header.Set("X-Real-Ip", "7.7.7.7")
	if got := s.clientIP(r); got != "7.7.7.7" {
		t.Fatalf("X-Real-IP fallback = %q", got)
	}
	// Proxy trust disabled: always key on the socket peer.
	s.cfg.TrustedProxyHops = -1
	r.Header.Set("X-Forwarded-For", "6.6.6.6")
	if got := s.clientIP(r); got != "10.0.0.9" {
		t.Fatalf("hops=-1 honored header: %q", got)
	}
}

// End-to-end: hammering /admin/login flips to 429 and stops burning bcrypt.
func TestLoginRateLimited(t *testing.T) {
	_, _, ts := newTestServer(t, true)
	form := url.Values{"password": {"wrong"}}
	var last int
	for i := 0; i < loginBurst+2; i++ {
		resp, err := http.Post(ts.URL+"/admin/login", "application/x-www-form-urlencoded",
			strings.NewReader(form.Encode()))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		last = resp.StatusCode
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("expected 429 after burst, got %d", last)
	}
}
