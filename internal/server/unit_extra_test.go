package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/stubbedev/xilo/internal/config"
	"github.com/stubbedev/xilo/internal/storage"
	"github.com/stubbedev/xilo/internal/store"
)

func TestFormSeconds(t *testing.T) {
	cases := []struct {
		value, unit string
		want        int64
		ok          bool
	}{
		{"2", "d", 172800, true},
		{"1", "y", 31536000, true},
		{"1", "mo", 2592000, true},
		{"3", "", 10800, true}, // hours by default
		{"0.5", "h", 1800, true},
		{"0", "d", 0, true},
		{"", "d", 0, false},
		{"  ", "d", 0, false},
		{"abc", "d", 0, false},
		{"-1", "d", 0, false},
		{"NaN", "d", 0, false},
		{"Inf", "d", 0, false},
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodGet, "/?"+url.Values{"x_value": {c.value}, "x_unit": {c.unit}}.Encode(), nil)
		got, ok := formSeconds(r, "x")
		if got != c.want || ok != c.ok {
			t.Errorf("formSeconds(%q,%q) = %d,%v want %d,%v", c.value, c.unit, got, ok, c.want, c.ok)
		}
	}
}

func TestFormBytes(t *testing.T) {
	cases := []struct {
		value, unit string
		want        int64
		ok          bool
	}{
		{"1", "MiB", 1 << 20, true},
		{"1", "TiB", 1 << 40, true},
		{"2", "", 2 << 30, true}, // GiB by default
		{"0.5", "GiB", 1 << 29, true},
		{"0", "GiB", 0, true},
		{"", "GiB", 0, false},
		{"junk", "GiB", 0, false},
		{"-3", "GiB", 0, false},
		{"NaN", "GiB", 0, false},
		{"+Inf", "GiB", 0, false},
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodGet, "/?"+url.Values{"x_value": {c.value}, "x_unit": {c.unit}}.Encode(), nil)
		got, ok := formBytes(r, "x")
		if got != c.want || ok != c.ok {
			t.Errorf("formBytes(%q,%q) = %d,%v want %d,%v", c.value, c.unit, got, ok, c.want, c.ok)
		}
	}
}

func TestClampPriority(t *testing.T) {
	cases := []struct {
		in       string
		fallback int
		want     int
	}{
		{"", 40, 40},
		{"abc", 40, 40},
		{"0", 40, 40},
		{"-5", 40, 1},
		{"500", 40, 100},
		{"42", 40, 42},
		{" 7 ", 40, 7},
	}
	for _, c := range cases {
		if got := clampPriority(c.in, c.fallback); got != c.want {
			t.Errorf("clampPriority(%q,%d) = %d want %d", c.in, c.fallback, got, c.want)
		}
	}
}

func TestPwState(t *testing.T) {
	cases := []struct{ pw, confirm, want string }{
		{"", "", ""},
		{"short", "short", "short"},
		{"longenough", "different1", "mismatch"},
		{"lowercase", "", "weak"},
		{"Abcdef123456", "Abcdef123456", "strong"}, // 12 chars, 3 classes
		{"aaaaaaaaaaaaaaaa", "", "strong"},         // 16 chars beats class count
		{"Ab1!xyz9", "", "weak"},                   // 8 chars, 4 classes but <12
	}
	for _, c := range cases {
		if got := pwState(c.pw, c.confirm); got != c.want {
			t.Errorf("pwState(%q,%q) = %q want %q", c.pw, c.confirm, got, c.want)
		}
	}
}

func TestFuzzyMatch(t *testing.T) {
	cases := []struct {
		s, q string
		want bool
	}{
		{"my-cache", "", true},
		{"my-cache", "mc", true}, // subsequence
		{"my-cache", "MY CACHE", true},
		{"my-cache", "xyz", false},
		{"ci token prod", "ci prod", true},
		{"abc", "abcd", false},
	}
	for _, c := range cases {
		if got := fuzzyMatch(c.s, c.q); got != c.want {
			t.Errorf("fuzzyMatch(%q,%q) = %v want %v", c.s, c.q, got, c.want)
		}
	}
}

func TestSortTokensAndParams(t *testing.T) {
	toks := []store.Token{
		{Name: "b", Perms: []string{"push"}, Caches: []string{"z"}, Expires: 0},
		{Name: "a", Perms: []string{"pull"}, Caches: []string{"y"}, Expires: 100, Revoked: true},
		{Name: "C", Perms: []string{"pull", "push"}, Caches: []string{"x"}, Expires: 50},
	}
	sortTokens(toks, "name", "asc")
	if toks[0].Name != "a" || toks[1].Name != "b" || toks[2].Name != "C" {
		t.Errorf("name asc: %v %v %v", toks[0].Name, toks[1].Name, toks[2].Name)
	}
	sortTokens(toks, "expires", "asc")
	// expires=0 (never) must sort last
	if toks[2].Expires != 0 {
		t.Errorf("expires asc last = %d, want 0 (never)", toks[2].Expires)
	}
	sortTokens(toks, "expires", "desc")
	if toks[0].Expires != 0 {
		t.Errorf("expires desc first = %d, want 0 (never)", toks[0].Expires)
	}
	sortTokens(toks, "perms", "asc")
	sortTokens(toks, "scope", "asc")
	sortTokens(toks, "status", "asc")
	before := append([]store.Token(nil), toks...)
	sortTokens(toks, "", "asc") // no key: untouched
	for i := range toks {
		if toks[i].Name != before[i].Name {
			t.Fatal("empty sort key must not reorder")
		}
	}

	r := httptest.NewRequest(http.MethodGet, "/?s=perms&d=asc", nil)
	if k, d := sortParams(r, "s", "d", "name", "perms"); k != "perms" || d != "asc" {
		t.Errorf("sortParams = %q,%q", k, d)
	}
	r = httptest.NewRequest(http.MethodGet, "/?s=evil&d=up", nil)
	if k, d := sortParams(r, "s", "d", "name", "perms"); k != "" || d != "desc" {
		t.Errorf("sortParams whitelist = %q,%q", k, d)
	}
}

func TestPageParams(t *testing.T) {
	cases := []struct {
		url          string
		wantN, wantS int
	}{
		{"/", 1, 25},
		{"/?g[number]=3&g[size]=10", 3, 10},
		{"/?g[number]=-2&g[size]=0", 1, 25},
		{"/?g[size]=9999", 1, 200},
		{"/?g[number]=abc", 1, 25},
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodGet, c.url, nil)
		n, s := pageParams(r, "g", 25)
		if n != c.wantN || s != c.wantS {
			t.Errorf("pageParams(%q) = %d,%d want %d,%d", c.url, n, s, c.wantN, c.wantS)
		}
	}
}

func TestMakePager(t *testing.T) {
	p := makePager("/admin", map[string][]string{"q": {"x"}}, "g", 2, 5)
	if p.Prev == "" || p.Next == "" || !strings.Contains(p.Next, "g%5Bnumber%5D=3") || !strings.Contains(p.Prev, "q=x") {
		t.Errorf("pager mid: %+v", p)
	}
	p = makePager("/admin", nil, "g", 1, 1)
	if p.Prev != "" || p.Next != "" {
		t.Errorf("pager single page: %+v", p)
	}
	if wp := withTarget(p, "#list"); wp.Target != "#list" {
		t.Errorf("withTarget: %+v", wp)
	}
}

func TestSmallHelpers(t *testing.T) {
	// humanBytes
	for _, c := range []struct {
		n    int64
		want string
	}{{512, "512 B"}, {2048, "2.0 KiB"}, {5 << 30, "5.0 GiB"}, {3 << 40, "3.0 TiB"}} {
		if got := humanBytes(c.n); got != c.want {
			t.Errorf("humanBytes(%d) = %q want %q", c.n, got, c.want)
		}
	}
	// humanDur
	for _, c := range []struct {
		d    time.Duration
		want string
	}{
		{45 * time.Second, "45s"},
		{12 * time.Minute, "12m"},
		{3*time.Hour + 20*time.Minute, "3h 20m"},
		{5*24*time.Hour + 4*time.Hour, "5d 4h"},
	} {
		if got := humanDur(c.d); got != c.want {
			t.Errorf("humanDur(%v) = %q want %q", c.d, got, c.want)
		}
	}
	// hitPct
	if hitPct(0, 0) != "—" || hitPct(3, 1) != "75%" {
		t.Errorf("hitPct: %q %q", hitPct(0, 0), hitPct(3, 1))
	}
	if hitPct(999, 1) != "99.9%" || hitPct(1, 2) != "33.3%" {
		t.Errorf("hitPct decimals: %q %q", hitPct(999, 1), hitPct(1, 2))
	}
	// hostOf
	if hostOf("https://x.example.com/") != "x.example.com" || hostOf("bare") != "bare" {
		t.Errorf("hostOf: %q %q", hostOf("https://x.example.com/"), hostOf("bare"))
	}
	// zstdLevel
	for name, want := range map[string]zstd.EncoderLevel{
		"fastest": zstd.SpeedFastest, "better": zstd.SpeedBetterCompression,
		"best": zstd.SpeedBestCompression, "anything": zstd.SpeedDefault,
	} {
		if got := zstdLevel(name); got != want {
			t.Errorf("zstdLevel(%q) = %v want %v", name, got, want)
		}
	}
	// isCacheTraffic
	for path, want := range map[string]bool{
		"/":                        false,
		"/admin":                   false,
		"/admin/status":            false,
		"/static/x.css":            false,
		"/healthz":                 false,
		"/metrics":                 false,
		"/favicon.ico":             false,
		"/c/default/c/nar/abc.nar": true,
		"/c/default/c/api/config":  true,
	} {
		if got := isCacheTraffic(path); got != want {
			t.Errorf("isCacheTraffic(%q) = %v want %v", path, got, want)
		}
	}
}

func serveRecovering(h http.Handler, w http.ResponseWriter, r *http.Request) (rec any) {
	defer func() { rec = recover() }()
	h.ServeHTTP(w, r)
	return nil
}

func TestMiddlewarePanicAndLogging(t *testing.T) {
	s, _, _ := newTestServerCfg(t, nil)

	// panic before any write → 500
	h := s.middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	}))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/c/default/c/nar/x.nar", nil))
	if rr.Code != 500 {
		t.Fatalf("panic → %d want 500", rr.Code)
	}

	// panic after a write → the response is aborted, never finished cleanly
	h = s.middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte("partial"))
		panic("late boom")
	}))
	rr = httptest.NewRecorder()
	rec := serveRecovering(h, rr, httptest.NewRequest(http.MethodGet, "/c/default/c/nar/x.nar", nil))
	if err, _ := rec.(error); !errors.Is(err, http.ErrAbortHandler) {
		t.Fatalf("late panic propagated %v, want http.ErrAbortHandler", rec)
	}
	if rr.Code != 201 || rr.Body.String() != "partial" {
		t.Fatalf("late panic → %d %q", rr.Code, rr.Body.String())
	}

	// pull traffic counts as pull, push API as push; admin and scanner junk
	// count as neither
	before, beforePush := s.metrics.reqTotal.Load(), s.metrics.pushReq.Load()
	ok := s.middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("hi")) }))
	ok.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/c/default/c/nix-cache-info", nil))
	ok.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/c/default/c/api/config", nil))
	ok.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/admin", nil))
	ok.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/robots.txt", nil))
	if got := s.metrics.reqTotal.Load(); got != before+1 {
		t.Fatalf("reqTotal delta = %d want 1", got-before)
	}
	if got := s.metrics.pushReq.Load(); got != beforePush+1 {
		t.Fatalf("pushReq delta = %d want 1", got-beforePush)
	}
}

func TestRequestLogStreams(t *testing.T) {
	var stdout, stderr bytes.Buffer
	prevReqLog, prevOut := reqLog, log.Writer()
	reqLog = newRequestLog(&stdout, &stderr)
	log.SetOutput(&stderr)
	t.Cleanup(func() { reqLog = prevReqLog; log.SetOutput(prevOut) })

	for _, logging := range []string{"full", "quiet"} {
		s, _, _ := newTestServerCfg(t, func(c *config.Config) { c.Logging = logging })
		hit := func(path string, h http.HandlerFunc) {
			serveRecovering(s.middleware(h), httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/c/default/c/"+logging+"-"+path, nil))
		}
		status := func(code int) http.HandlerFunc {
			return func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(code) }
		}
		hit("ok", status(http.StatusOK))
		hit("redirect", status(http.StatusFound))
		hit("missing", status(http.StatusNotFound))
		hit("failed", status(http.StatusInternalServerError))
		hit("panic", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusCreated)
			panic("late boom")
		})
	}

	if !strings.Contains(stderr.String(), "full-failed ") || !strings.Contains(stderr.String(), "full-panic ") {
		t.Errorf("a 5xx or a panic waited for a flush\nstderr: %s", &stderr)
	}
	if missing, failed := strings.Index(stderr.String(), "full-missing "), strings.Index(stderr.String(), "full-failed "); missing < 0 || missing > failed {
		t.Errorf("the 404 before a 500 is not ahead of it on stderr\nstderr: %s", &stderr)
	}
	reqLog.flush()

	for path, want := range map[string]string{
		"full-ok":       "stdout",
		"full-redirect": "stdout",
		"full-missing":  "stderr",
		"full-failed":   "stderr",
		"full-panic":    "stderr",
		"quiet-ok":      "",
		"quiet-missing": "stderr",
		"quiet-failed":  "stderr",
		"quiet-panic":   "stderr",
	} {
		got := ""
		switch {
		case strings.Contains(stdout.String(), path+" "):
			got = "stdout"
		case strings.Contains(stderr.String(), path+" "):
			got = "stderr"
		}
		if got != want || (strings.Contains(stdout.String(), path+" ") && strings.Contains(stderr.String(), path+" ")) {
			t.Errorf("%s logged to %q want %q\nstdout: %s\nstderr: %s", path, got, want, &stdout, &stderr)
		}
	}
}

func TestRequestLogCollectsHealthyLines(t *testing.T) {
	var stdout, stderr bytes.Buffer
	l := requestLog{
		out:  &lineBuffer{w: &stdout, every: time.Hour},
		errs: &lineBuffer{w: &stderr, every: time.Hour},
	}
	l.request("GET", "/ok", 200, 0, false, false)
	l.request("GET", "/missing", 404, 0, false, false)
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("lines written before a flush\nstdout: %s\nstderr: %s", &stdout, &stderr)
	}
	l.request("GET", "/failed", 500, 0, false, false)
	if got, want := stderr.String(), "GET /missing 404 0s\nGET /failed 500 0s\n"; got != want {
		t.Fatalf("stderr after a 500 = %q want %q", got, want)
	}
	if stdout.Len() != 0 {
		t.Fatalf("a 500 flushed stdout: %s", &stdout)
	}
	l.flush()
	if got, want := stdout.String(), "GET /ok 200 0s\n"; got != want {
		t.Fatalf("stdout after a flush = %q want %q", got, want)
	}

	var full bytes.Buffer
	b := &lineBuffer{w: &full, every: time.Hour}
	for full.Len() == 0 {
		b.request("GET", "/a-path-long-enough-to-fill-the-buffer-in-few-lines", 200, 0, false)
	}
	if full.Len() < logFlushBytes {
		t.Fatalf("buffer flushed at %d bytes, before reaching %d", full.Len(), logFlushBytes)
	}
}

func TestRequestLineFormat(t *testing.T) {
	for _, d := range []time.Duration{
		0,
		time.Microsecond,
		time.Millisecond,
		time.Millisecond + 500*time.Microsecond,
		17 * time.Millisecond,
		time.Second,
		time.Second + time.Millisecond,
		time.Second + 50*time.Millisecond,
		59 * time.Second,
		time.Minute + 1500*time.Millisecond,
		10 * time.Minute,
		time.Hour + time.Second + 5*time.Millisecond,
		2540400 * time.Hour,
	} {
		want := "GET /x 200 " + d.Round(time.Millisecond).String() + "\n"
		if got := string(appendRequestLine(nil, "GET", "/x", 200, d)); got != want {
			t.Errorf("d=%v: got %q want %q", d, got, want)
		}
	}
	rng := rand.New(rand.NewSource(1))
	for range 10000 {
		d := time.Duration(rng.Int63n(int64(48 * time.Hour)))
		want := "POST /p 404 " + d.Round(time.Millisecond).String() + "\n"
		if got := string(appendRequestLine(nil, "POST", "/p", 404, d)); got != want {
			t.Fatalf("d=%v: got %q want %q", d, got, want)
		}
	}
}

func TestRequestLogAllocations(t *testing.T) {
	l := newRequestLog(io.Discard, io.Discard)
	run := func() {
		l.request("GET", "/c/a/c/nix/store/0aaaa-hello-2.12.1.drv", 200, 3*time.Millisecond, false, false)
		l.request("POST", "/admin/orgs", 500, time.Second+time.Millisecond, false, true)
	}
	run()
	run()
	// The old log.Printf line allocated on every request, so only a per-line
	// regression may fail here. A stray alloc from a goroutine an earlier test
	// left behind can land in the measured window, which -race builds count
	// too; a thousand pairs amortize it below the bar.
	if n := testing.AllocsPerRun(1000, run); n > 1.5 {
		t.Errorf("allocs per request pair = %v, want below 1.5", n)
	}
}

func BenchmarkRequestLog(b *testing.B) {
	line := "/c/a/c/nix/store/0aaaa-hello-2.12.1.drv"
	b.Run("sink", func(b *testing.B) {
		l := newRequestLog(io.Discard, io.Discard)
		b.ReportAllocs()
		for b.Loop() {
			l.request("GET", line, 200, 3*time.Millisecond, false, false)
		}
	})
	b.Run("log", func(b *testing.B) {
		l := log.New(io.Discard, "", log.LstdFlags)
		b.ReportAllocs()
		for b.Loop() {
			l.Printf("%s %s %d %s", "GET", line, 200, 3*time.Millisecond)
		}
	})
}

func TestRunContextShutdown(t *testing.T) {
	s, _, _ := newTestServerCfg(t, func(cfg *config.Config) {
		cfg.Listen = "127.0.0.1:0"
		cfg.GC.Interval = "1h"
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already stopped: RunContext must shut down cleanly
	if err := s.RunContext(ctx); err != nil {
		t.Fatalf("RunContext: %v", err)
	}
}

// ---- streamchunks ----

func TestEachOrdered(t *testing.T) {
	mkRefs := func(n int) []store.ChunkRef {
		refs := make([]store.ChunkRef, n)
		for i := range refs {
			refs[i] = store.ChunkRef{Key: fmt.Sprintf("k%d", i)}
		}
		return refs
	}
	fetch := func(_ context.Context, ref store.ChunkRef) ([]byte, error) {
		return []byte(ref.Key), nil
	}

	for _, ahead := range []int{0, 1, 3, 100} {
		var got []string
		err := eachOrdered(context.Background(), mkRefs(7), window{depth: ahead}, fetch, func(b []byte) error {
			got = append(got, string(b))
			return nil
		})
		if err != nil {
			t.Fatalf("ahead=%d: %v", ahead, err)
		}
		for i, g := range got {
			if g != fmt.Sprintf("k%d", i) {
				t.Fatalf("ahead=%d out of order at %d: %v", ahead, i, got)
			}
		}
		if len(got) != 7 {
			t.Fatalf("ahead=%d: %d chunks", ahead, len(got))
		}
	}

	// fetch error mid-stream propagates; earlier chunks were delivered in order
	boom := errors.New("fetch failed")
	failFetch := func(_ context.Context, ref store.ChunkRef) ([]byte, error) {
		if ref.Key == "k3" {
			return nil, boom
		}
		return []byte(ref.Key), nil
	}
	var seen int
	err := eachOrdered(context.Background(), mkRefs(7), window{depth: 2}, failFetch, func(b []byte) error {
		seen++
		return nil
	})
	if !errors.Is(err, boom) || seen != 3 {
		t.Fatalf("fetch error: err=%v seen=%d", err, seen)
	}

	// consumer error propagates
	sink := errors.New("consumer refused")
	err = eachOrdered(context.Background(), mkRefs(3), window{depth: 2}, fetch, func([]byte) error { return sink })
	if !errors.Is(err, sink) {
		t.Fatalf("consumer error: %v", err)
	}

	// empty refs: no calls, no error
	if err := eachOrdered(context.Background(), nil, window{depth: 4}, fetch, func([]byte) error {
		t.Fatal("fn called for empty refs")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

type slowChunkReader struct {
	data []byte
	err  error
}

func (r *slowChunkReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		if r.err != nil {
			return 0, r.err
		}
		return 0, io.EOF
	}
	p[0] = r.data[0] // one byte at a time: exercises the grow loop
	r.data = r.data[1:]
	return 1, nil
}

func TestReadAllInto(t *testing.T) {
	content := []byte("twelve bytes")
	cases := []struct {
		name string
		buf  []byte
	}{
		{"zero cap", nil},
		{"exact cap", make([]byte, 0, len(content))},
		{"oversized cap", make([]byte, 0, 64)},
		{"undersized cap", make([]byte, 0, 3)},
	}
	for _, c := range cases {
		got, err := readAllInto(c.buf, &slowChunkReader{data: append([]byte(nil), content...)})
		if err != nil || string(got) != string(content) {
			t.Errorf("%s: %q err=%v", c.name, got, err)
		}
	}
	// non-EOF error propagates with partial data
	boom := errors.New("disk gone")
	got, err := readAllInto(nil, &slowChunkReader{data: []byte("ab"), err: boom})
	if !errors.Is(err, boom) || string(got) != "ab" {
		t.Errorf("error case: %q err=%v", got, err)
	}
}

func TestEachOrderedBorrowsOnlyWhatIsFree(t *testing.T) {
	refs := make([]store.ChunkRef, 40)
	inFlight := func(w window) int {
		t.Helper()
		var now, peak atomic.Int64
		release := make(chan struct{})
		fetch := func(context.Context, store.ChunkRef) ([]byte, error) {
			if n := now.Add(1); n > peak.Load() {
				peak.Store(n)
			}
			<-release
			return nil, nil
		}
		done := make(chan error, 1)
		go func() {
			done <- eachOrdered(context.Background(), refs, w, fetch, func([]byte) error {
				now.Add(-1)
				return nil
			})
		}()
		for now.Load() < int64(w.depth) {
			runtime.Gosched()
		}
		time.Sleep(20 * time.Millisecond)
		filled := int(now.Load())
		close(release)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		return filled
	}

	shared := make(chan struct{}, 5)
	if got := inFlight(window{depth: 2, extra: 3, shared: shared}); got != 5 {
		t.Errorf("in flight with 3 to borrow = %d want 5", got)
	}
	if len(shared) != 0 {
		t.Errorf("%d borrowed slots not given back", len(shared))
	}
	if got := inFlight(window{depth: 2, extra: 10, shared: shared}); got != 7 {
		t.Errorf("in flight with all 5 shared slots free = %d want 7", got)
	}
	for range cap(shared) {
		shared <- struct{}{}
	}
	if got := inFlight(window{depth: 2, extra: 3, shared: shared}); got != 2 {
		t.Errorf("in flight with nothing free = %d want its own 2", got)
	}
	if len(shared) != cap(shared) {
		t.Errorf("a stream that borrowed nothing took %d slots back", cap(shared)-len(shared))
	}
}

// networkRoot is a local backend standing on a network mount: it serves
// files like any local root but reports itself latency-bound, as NewLocal
// does for a CIFS or NFS root. It counts how chunks were read.
type networkRoot struct {
	*storage.Local
	gets, opens atomic.Int64
}

func (n *networkRoot) LatencyBound() bool { return true }

func (n *networkRoot) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	n.gets.Add(1)
	return n.Local.Get(ctx, key)
}

func (n *networkRoot) Open(key string) (*os.File, error) {
	n.opens.Add(1)
	return n.Local.Open(key)
}

func TestReadAhead(t *testing.T) {
	local, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if local.LatencyBound() {
		t.Fatal("a temp dir reports itself latency-bound")
	}
	s3, err := storage.NewS3(config.S3{Endpoint: "s3.invalid", Bucket: "b"})
	if err != nil {
		t.Fatal(err)
	}
	srv := func(par int) *Server {
		return &Server{
			cfg:         &config.Config{Parallelism: par},
			sts:         map[string]storage.Storage{"default": local, "net": &networkRoot{Local: local}, "s3": s3},
			sharedAhead: make(chan struct{}, sharedAhead),
		}
	}
	for _, c := range []struct {
		backend      string
		par          int
		depth, extra int
	}{
		{"default", 16, 4, 0},
		{"default", 1, 4, 0},
		{"s3", 16, 16, 0},
		{"s3", 1, 4, 12},
		{"net", 1, 4, 12},
		{"net", 16, 16, 0},
	} {
		w := srv(c.par).readAhead(c.backend)
		if w.depth != c.depth || w.extra != c.extra {
			t.Errorf("readAhead(%s) at parallelism %d = %d+%d want %d+%d", c.backend, c.par, w.depth, w.extra, c.depth, c.extra)
		}
		if c.extra > 0 && cap(w.shared) != sharedAhead {
			t.Errorf("readAhead(%s) borrows from a pool of %d want %d", c.backend, cap(w.shared), sharedAhead)
		}
	}
}

// A zstd NAR on a latency-bound file backend must go through the prefetching
// reader, not the sequential sendfile path: on a CIFS root the latter pays one
// round trip per chunk in series.
func TestNetworkRootServesThroughPrefetch(t *testing.T) {
	var root *networkRoot
	_, db, ts := newTestServerWith(t, nil, func(base storage.Storage) storage.Storage {
		root = &networkRoot{Local: base.(*storage.Local)}
		return root
	})
	if _, err := db.CreateCache("default", "c", true, 40); err != nil {
		t.Fatal(err)
	}
	parts := [][]byte{bytes.Repeat([]byte("a"), 4096), bytes.Repeat([]byte("b"), 4096), bytes.Repeat([]byte("c"), 4096)}
	pushChunked(t, ts, "c", h32, parts, nil, "")
	gets := root.gets.Load()
	if got := getNar(t, ts, "/c/default/c/nar/"+h32+".nar", "zstd"); !bytes.Equal(got, bytes.Join(parts, nil)) {
		t.Fatal("zstd NAR from a network root does not match what was pushed")
	}
	if n := root.opens.Load(); n != 0 {
		t.Errorf("opened %d chunk files for sendfile on a latency-bound root", n)
	}
	if n := root.gets.Load() - gets; n != int64(len(parts)) {
		t.Errorf("read %d chunks through the prefetcher want %d", n, len(parts))
	}
}

// ---- status internals ----

func TestBucketMax(t *testing.T) {
	in := []float64{1, 5, 3, 2, 4}
	got := bucketMax(in, 2) // aligned from the newest edge: [1] [5,3] [2,4]
	want := []float64{1, 5, 4}
	if len(got) != len(want) {
		t.Fatalf("bucketMax len %d want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("bucketMax = %v want %v", got, want)
		}
	}
	if out := bucketMax(in, 1); &out[0] != &in[0] {
		t.Error("per<=1 should return input unchanged")
	}
}

func TestStatusRangeAndRate(t *testing.T) {
	get := func(q string) statusRangeQ {
		return statusRange(httptest.NewRequest(http.MethodGet, "/?"+q, nil))
	}
	if q := get("window=30"); q.WinMin != 30 || q.Custom {
		t.Errorf("preset: %+v", q)
	}
	if q := get("window=99999"); q.WinMin != 60 {
		t.Errorf("bad preset should default: %+v", q)
	}
	if q := get(""); q.WinMin != 60 {
		t.Errorf("default: %+v", q)
	}
	q := get("from=2026-01-01&to=2026-01-03")
	if !q.Custom || !q.To.After(q.From) {
		t.Errorf("custom: %+v", q)
	}
	if q := get("from=2026-01-05&to=2026-01-01"); q.Custom {
		t.Errorf("reversed range must not be custom: %+v", q)
	}
	if q := get("from=garbage&to=2026-01-01"); q.Custom {
		t.Errorf("bad from must not be custom: %+v", q)
	}

	for qs, want := range map[string]int{"rate=2": 2, "rate=10": 10, "rate=30": 30, "rate=60": 60, "rate=7": 5, "": 5} {
		if got := statusRate(httptest.NewRequest(http.MethodGet, "/?"+qs, nil)); got != want {
			t.Errorf("statusRate(%q) = %d want %d", qs, got, want)
		}
	}
}

func TestStatusSeriesWindows(t *testing.T) {
	s, db, _ := newTestServerCfg(t, nil)

	// live ring path
	s.metrics.reqTotal.Add(10)
	s.metrics.reqDurNs.Add(int64(50 * time.Millisecond))
	s.metrics.narBytes.Add(1 << 20)
	s.sampleStatus()
	s.sampleStatus()
	set := s.statusSeries(statusRangeQ{WinMin: 10})
	if len(set.req) == 0 || len(set.times) != len(set.req) {
		t.Fatalf("ring series: %d pts, %d times", len(set.req), len(set.times))
	}

	// rollup path: seed persisted minutes inside a custom window
	base := time.Now().Add(-24 * time.Hour).Truncate(time.Minute).Unix()
	for i := range int64(5) {
		if err := db.AddMetricMinute(store.MetricMinute{TS: base + i*60, Req: float64(i + 1), Lat: 2, Bps: 3, Stored: 4}); err != nil {
			t.Fatal(err)
		}
	}
	set = s.statusSeries(statusRangeQ{WinMin: 1440}) // beyond the ring
	if len(set.req) != drawnPoints {
		t.Fatalf("rollup series: %d pts want %d", len(set.req), drawnPoints)
	}
	if maxOf(set.req) < 5 {
		t.Fatalf("rollup series lost data: max %v", maxOf(set.req))
	}

	// custom historical range (no live patch)
	from := time.Unix(base, 0).Truncate(24 * time.Hour)
	q := statusRangeQ{Custom: true, From: from, To: from.AddDate(0, 0, 1)}
	set = s.statusSeries(q)
	if set.fromStr == "" || set.toStr == "" {
		t.Fatalf("custom range strings: %+v", set)
	}

	// full JSON payload over the rollup window
	sj := s.buildStatusJSON(statusRangeQ{WinMin: 1440})
	if len(sj.Charts) != 4 || sj.MaxT <= sj.MinT {
		t.Fatalf("status json: %d charts, range %d..%d", len(sj.Charts), sj.MinT, sj.MaxT)
	}
	for name, ch := range sj.Charts {
		if len(ch.Points) != drawnPoints {
			t.Errorf("chart %s: %d points want %d", name, len(ch.Points), drawnPoints)
		}
	}

	// page view model
	d := s.statusData(context.Background(), statusRangeQ{WinMin: 10})
	if len(d.Charts) != 4 || !d.Healthy {
		t.Fatalf("status data: %+v", d)
	}
}
