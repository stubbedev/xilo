package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/stubbedev/xilo/internal/config"
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
		r := httptest.NewRequest("GET", "/?"+url.Values{"x_value": {c.value}, "x_unit": {c.unit}}.Encode(), nil)
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
		r := httptest.NewRequest("GET", "/?"+url.Values{"x_value": {c.value}, "x_unit": {c.unit}}.Encode(), nil)
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

	r := httptest.NewRequest("GET", "/?s=perms&d=asc", nil)
	if k, d := sortParams(r, "s", "d", "name", "perms"); k != "perms" || d != "asc" {
		t.Errorf("sortParams = %q,%q", k, d)
	}
	r = httptest.NewRequest("GET", "/?s=evil&d=up", nil)
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
		r := httptest.NewRequest("GET", c.url, nil)
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
	// parseDur / parseDurSafe
	if parseDur("") != 0 || parseDur("0") != 0 || parseDur("bogus") != 0 || parseDur("5m") != 5*time.Minute {
		t.Error("parseDur table failed")
	}
	if parseDurSafe("", time.Hour) != 0 || parseDurSafe("bogus", time.Hour) != time.Hour || parseDurSafe("30m", time.Hour) != 30*time.Minute {
		t.Error("parseDurSafe table failed")
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

func TestMiddlewarePanicAndLogging(t *testing.T) {
	s, _, _ := newTestServerCfg(t, nil)

	// panic before any write → 500
	h := s.middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	}))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/c/default/c/nar/x.nar", nil))
	if rr.Code != 500 {
		t.Fatalf("panic → %d want 500", rr.Code)
	}

	// panic after a write → response stays as written
	h = s.middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte("partial"))
		panic("late boom")
	}))
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/c/default/c/nar/x.nar", nil))
	if rr.Code != 201 || rr.Body.String() != "partial" {
		t.Fatalf("late panic → %d %q", rr.Code, rr.Body.String())
	}

	// cache traffic counts toward metrics, admin traffic doesn't
	before := s.metrics.reqTotal.Load()
	ok := s.middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("hi")) }))
	ok.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/c/default/c/nix-cache-info", nil))
	ok.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/admin", nil))
	if got := s.metrics.reqTotal.Load(); got != before+1 {
		t.Fatalf("reqTotal delta = %d want 1", got-before)
	}
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
		err := eachOrdered(context.Background(), mkRefs(7), ahead, fetch, func(b []byte) error {
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
	err := eachOrdered(context.Background(), mkRefs(7), 2, failFetch, func(b []byte) error {
		seen++
		return nil
	})
	if !errors.Is(err, boom) || seen != 3 {
		t.Fatalf("fetch error: err=%v seen=%d", err, seen)
	}

	// consumer error propagates
	sink := errors.New("consumer refused")
	err = eachOrdered(context.Background(), mkRefs(3), 2, fetch, func([]byte) error { return sink })
	if !errors.Is(err, sink) {
		t.Fatalf("consumer error: %v", err)
	}

	// empty refs: no calls, no error
	if err := eachOrdered(context.Background(), nil, 4, fetch, func([]byte) error {
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

func TestReadAhead(t *testing.T) {
	// Local backend: shallow fixed window (no GET latency to hide).
	s, _, _ := newTestServerCfg(t, func(cfg *config.Config) { cfg.Parallelism = 16 })
	if s.readAhead() != 4 {
		t.Errorf("local readAhead = %d want 4", s.readAhead())
	}
	// S3: deep window scaled to parallelism, floor 4. (Client construction
	// needs endpoint+bucket but performs no network I/O.)
	s3cfg := func(par int) func(*config.Config) {
		return func(cfg *config.Config) {
			cfg.Storage.Backend = "s3"
			cfg.Storage.S3.Endpoint = "s3.invalid"
			cfg.Storage.S3.Bucket = "b"
			cfg.Parallelism = par
		}
	}
	s2, _, _ := newTestServerCfg(t, s3cfg(16))
	if s2.readAhead() != 16 {
		t.Errorf("s3 readAhead = %d want 16", s2.readAhead())
	}
	s3, _, _ := newTestServerCfg(t, s3cfg(1))
	if s3.readAhead() != 4 {
		t.Errorf("s3 readAhead floor = %d want 4", s3.readAhead())
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
		return statusRange(httptest.NewRequest("GET", "/?"+q, nil))
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
		if got := statusRate(httptest.NewRequest("GET", "/?"+qs, nil)); got != want {
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
	for i := int64(0); i < 5; i++ {
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
	d := s.statusData(statusRangeQ{WinMin: 10})
	if len(d.Charts) != 4 || !d.Healthy {
		t.Fatalf("status data: %+v", d)
	}
}
