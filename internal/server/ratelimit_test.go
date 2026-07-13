package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
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
	r := httptest.NewRequest(http.MethodPost, "/admin/login", nil)
	r.RemoteAddr = "10.1.2.3:5555"
	if got := clientIP(r); got != "10.1.2.3" {
		t.Fatalf("clientIP = %q", got)
	}
	r.RemoteAddr = "weird"
	if got := clientIP(r); got != "weird" {
		t.Fatalf("clientIP fallback = %q", got)
	}
	// Direct connection from a PUBLIC peer: forwarding headers are forgeable
	// and must be ignored — the socket peer wins.
	r.Header.Set("X-Forwarded-For", "6.6.6.6")
	r.Header.Set("X-Real-Ip", "7.7.7.7")
	r.RemoteAddr = "203.0.113.9:5555"
	if got := clientIP(r); got != "203.0.113.9" {
		t.Fatalf("public peer honored forgeable header: %q", got)
	}
	// Behind a proxy on a private/loopback peer: X-Real-IP wins.
	r.RemoteAddr = "10.0.0.9:5555"
	if got := clientIP(r); got != "7.7.7.7" {
		t.Fatalf("proxy peer X-Real-IP = %q", got)
	}
	// Loopback peer, no X-Real-IP: leftmost X-Forwarded-For entry wins.
	r.Header.Del("X-Real-Ip")
	r.Header.Set("X-Forwarded-For", "6.6.6.6, 10.0.0.9")
	r.RemoteAddr = "127.0.0.1:5555"
	if got := clientIP(r); got != "6.6.6.6" {
		t.Fatalf("proxy peer XFF = %q", got)
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
