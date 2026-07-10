package server

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stubbedev/xilo/internal/config"
	"github.com/stubbedev/xilo/internal/storage"
	"github.com/stubbedev/xilo/internal/store"
)

// TestClosedDBErrorPaths builds a server by hand (no cleanup-Close) so the DB
// can be shut down mid-test to reach the read-error branches.
func TestClosedDBErrorPaths(t *testing.T) {
	dir := t.TempDir()
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	cfg.DataDir = dir
	cfg.BaseURL = "http://example"
	cfg.Storage.Local.Root = filepath.Join(dir, "storage")
	db, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	st, err := storage.New(cfg.Storage)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(cfg, db, st)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	db.Close() // reads now fail

	resp, _ := http.Get(ts.URL + "/healthz")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("healthz on dead db → %d want 503", resp.StatusCode)
	}
	resp.Body.Close()
	resp, _ = http.Get(ts.URL + "/healthz?format=json")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("healthz json on dead db → %d want 503", resp.StatusCode)
	}
	resp.Body.Close()
	resp, _ = http.Get(ts.URL + "/c/nix-cache-info")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("cache lookup on dead db → %d want 500", resp.StatusCode)
	}
	resp.Body.Close()
	resp, _ = http.PostForm(ts.URL+"/admin/login", url.Values{"password": {"x"}})
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("login on dead db → %d want 500", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestNarServeAfterStorageLoss exercises the chunk-fetch error branches: the
// DB row exists but the blob is gone.
func TestNarServeAfterStorageLoss(t *testing.T) {
	s, db, ts := newTestServerCfg(t, nil)
	db.CreateCache("c", true, 40)
	pushFake(t, ts, "c", h32, []byte("bytes that will vanish"), "")
	if err := os.RemoveAll(s.cfg.Storage.Local.Root); err != nil {
		t.Fatal(err)
	}
	// Headers may already be committed when the fetch fails; we only care that
	// the server survives and the stream stops. Body/transport errors are fine.
	for _, enc := range []string{"identity", "zstd", "gzip"} {
		resp, err := http.DefaultClient.Do(func() *http.Request {
			req, _ := http.NewRequest("GET", ts.URL+"/c/nar/"+h32+".nar", nil)
			req.Header.Set("Accept-Encoding", enc)
			return req
		}())
		if err == nil {
			resp.Body.Close()
		}
	}
	// registering another path over the same (now blobless) chunk fails
	// reassembly verification cleanly.
	data := []byte("bytes that will vanish")
	ch, narHash, narSize := fakeNar(data)
	pr, _ := json.Marshal(map[string]any{
		"storePath": "/nix/store/" + h32b + "-again", "narHash": narHash,
		"narSize": narSize, "chunks": []string{ch},
	})
	if r := put(t, ts, "/c/api/path", pr, ""); r.StatusCode != 400 {
		t.Errorf("put path over lost blob → %d want 400", r.StatusCode)
	}

	// still healthy afterwards
	resp, _ := http.Get(ts.URL + "/healthz")
	if resp.StatusCode != 200 {
		t.Fatalf("server unhealthy after storage loss: %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestDirectSmallGaps(t *testing.T) {
	// logWriter.Unwrap
	rec := httptest.NewRecorder()
	lw := &logWriter{ResponseWriter: rec}
	if lw.Unwrap() != http.ResponseWriter(rec) {
		t.Error("Unwrap should return the inner writer")
	}

	// passkeyName strips the port
	s, _, _ := newTestServerCfg(t, func(cfg *config.Config) { cfg.BaseURL = "https://cache.example.com:8443" })
	if got := s.passkeyName(); got != "cache.example.com - xilo" {
		t.Errorf("passkeyName = %q", got)
	}

	// maxChunkBody uses NarThreshold when it exceeds MaxSize
	s2, _, _ := newTestServerCfg(t, func(cfg *config.Config) {
		cfg.Chunking.MaxSize = 1 << 20
		cfg.Chunking.NarThreshold = 8 << 20
	})
	if got := s2.maxChunkBody(); got != (8<<20)+(1<<20) {
		t.Errorf("maxChunkBody = %d", got)
	}

	// rangeSeries with an empty/inverted window
	if r, _, _, _, _ := s.rangeSeries(100, 100); r != nil {
		t.Error("rangeSeries(to<=from) should be nil")
	}

	// statusChartData with no samples
	if c := statusChartData("x", "status.req", nil, fmtReq); c.Cur != "" || c.Peak != "" {
		t.Errorf("empty chart data: %+v", c)
	}

	// totpQRDataURI: payload too large to encode
	if _, err := totpQRDataURI(strings.Repeat("x", 5000)); err == nil {
		t.Error("giant QR payload should fail")
	}
}

func TestSampleStatusMinuteFlush(t *testing.T) {
	s, db, _ := newTestServerCfg(t, nil)
	past := (time.Now().Unix()/60 - 5) * 60
	s.stat.mu.Lock()
	s.stat.curMin = past
	s.stat.minReq = 42
	s.stat.mu.Unlock()
	s.sampleStatus() // minute rolled over → flush the finished minute

	deadline := time.Now().Add(2 * time.Second)
	for {
		rows, err := db.MetricRange(past, past+60)
		if err == nil && len(rows) == 1 && rows[0].Req == 42 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("flushed minute never persisted: %v err=%v", rows, err)
		}
		time.Sleep(5 * time.Millisecond) // ponytail: async flush, poll with deadline
	}
}

func TestAdminGatesAnonymousAndMissing(t *testing.T) {
	_, db, ts := newTestServerCfg(t, nil)
	bootstrapAdmin(t, db)

	// every mutating admin route bounces anonymous callers to the login page
	nr := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for _, p := range []string{
		"/admin/gc", "/admin/cache/x/rotate", "/admin/cache/x/delete",
		"/admin/cache/x/configure", "/admin/tokens", "/admin/tokens/1/edit",
		"/admin/tokens/1/revoke", "/admin/settings/password",
		"/admin/settings/totp/enroll", "/admin/settings/totp/enable",
		"/admin/settings/totp/disable", "/admin/passkeys/register/begin",
		"/admin/passkeys/register/finish", "/admin/passkeys/1/delete",
	} {
		resp, err := nr.Post(ts.URL+p, "application/x-www-form-urlencoded", nil)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusSeeOther {
			t.Errorf("anon POST %s → %d want 303", p, resp.StatusCode)
		}
		resp.Body.Close()
	}
	for _, p := range []string{"/admin/status", "/admin/cache/x", "/admin/settings"} {
		resp, _ := nr.Get(ts.URL + p)
		if resp.StatusCode != http.StatusSeeOther {
			t.Errorf("anon GET %s → %d want 303", p, resp.StatusCode)
		}
		resp.Body.Close()
	}

	// logged in, but the target cache doesn't exist
	c := adminClient(t, ts)
	for _, p := range []string{"/admin/cache/ghost/rotate", "/admin/cache/ghost/delete"} {
		resp, _ := c.PostForm(ts.URL+p, nil)
		if resp.StatusCode != 404 {
			t.Errorf("POST %s → %d want 404", p, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

func TestCreateAndEditTokenDefaults(t *testing.T) {
	_, db, ts := newTestServerCfg(t, nil)
	bootstrapAdmin(t, db)
	c := adminClient(t, ts)

	// no perms, no cache, no ttl → pull-only, all caches, never expires
	resp, _ := c.PostForm(ts.URL+"/admin/tokens", url.Values{"name": {"minimal"}, "cache": {"*"}})
	resp.Body.Close()
	toks, _ := db.ListTokens()
	if len(toks) != 1 {
		t.Fatalf("tokens: %+v", toks)
	}
	tok := toks[0]
	if len(tok.Perms) != 1 || tok.Perms[0] != "pull" || tok.Expires != 0 {
		t.Fatalf("default token: %+v", tok)
	}

	// edit: blank name keeps old, push perm, fresh TTL
	id := tok.ID
	resp, _ = c.PostForm(ts.URL+"/admin/tokens/"+strconv.FormatInt(id, 10)+"/edit", url.Values{
		"name": {""}, "cache": {"somecache"}, "push": {"on"},
		"ttl_value": {"1"}, "ttl_unit": {"h"},
	})
	resp.Body.Close()
	got, _ := db.GetToken(id)
	if got.Name != "minimal" || got.Expires <= time.Now().Unix() || len(got.Perms) != 1 || got.Perms[0] != "push" {
		t.Fatalf("edited token: %+v", got)
	}
}

func TestWebAuthnBadRelyingParty(t *testing.T) {
	// default test BaseURL "http://example" is not a valid RP domain — every
	// webauthn route must surface the config error, not panic.
	_, db, ts := newTestServerCfg(t, nil)
	bootstrapAdmin(t, db)
	c := adminClient(t, ts)
	for _, p := range []string{
		"/admin/login/passkey/begin", "/admin/login/passkey/finish",
		"/admin/passkeys/register/begin", "/admin/passkeys/register/finish",
	} {
		resp, _ := c.Post(ts.URL+p, "application/json", strings.NewReader("{}"))
		if resp.StatusCode != http.StatusInternalServerError {
			t.Errorf("bad RP %s → %d want 500", p, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

func TestRunContextListenError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	s, _, _ := newTestServerCfg(t, func(cfg *config.Config) { cfg.Listen = ln.Addr().String() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.RunContext(ctx); err == nil {
		t.Fatal("RunContext on an occupied port should fail")
	}
}

func TestStartGCDisabled(t *testing.T) {
	s, _, _ := newTestServerCfg(t, nil) // no gc.interval → no-op
	s.startGC(context.Background())
}

func TestStartGCSweepsOnTick(t *testing.T) {
	s, db, ts := newTestServerCfg(t, func(cfg *config.Config) {
		cfg.GC.Interval = "1ms"
		cfg.GC.Grace = "-1s"
	})
	db.CreateCache("c", true, 40)
	// orphan chunk: uploaded, never referenced by a path
	data := []byte("orphan chunk")
	ch, _, _ := fakeNar(data)
	if r := put(t, ts, "/c/api/chunk/"+ch, data, ""); r.StatusCode != 200 {
		t.Fatalf("put chunk: %d", r.StatusCode)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.startGC(ctx)
	deadline := time.Now().Add(3 * time.Second)
	for {
		g, err := db.GlobalStats()
		if err == nil && g.StoredBytes == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("background GC never swept the orphan chunk (%d bytes left)", g.StoredBytes)
		}
		time.Sleep(2 * time.Millisecond) // ponytail: ticker-driven sweep, poll with deadline
	}
}

func TestAPIUnknownCache404(t *testing.T) {
	_, _, ts := newTestServerCfg(t, nil)
	for _, p := range []string{"/nope/api/get-missing-paths", "/nope/api/get-missing-chunks"} {
		resp, _ := http.Post(ts.URL+p, "application/json", strings.NewReader(`{"hashes":[]}`))
		if resp.StatusCode != 404 {
			t.Errorf("POST %s → %d want 404", p, resp.StatusCode)
		}
		resp.Body.Close()
	}
	if r := put(t, ts, "/nope/api/chunk/"+strings.Repeat("0", 64), []byte("x"), ""); r.StatusCode != 404 {
		t.Errorf("put chunk unknown cache → %d want 404", r.StatusCode)
	}
	if r := put(t, ts, "/nope/api/path", []byte("{}"), ""); r.StatusCode != 404 {
		t.Errorf("put path unknown cache → %d want 404", r.StatusCode)
	}
	resp, _ := http.Get(ts.URL + "/nope/" + h32 + ".narinfo")
	if resp.StatusCode != 404 {
		t.Errorf("narinfo unknown cache → %d want 404", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestPrivateCachePullGates(t *testing.T) {
	_, db, ts := newTestServerCfg(t, func(cfg *config.Config) { cfg.Security.AllowOpenBootstrap = false })
	db.CreateCache("priv", false, 40)
	db.CreateToken("t", nil, []string{"push"}, 0)
	for _, p := range []string{"/priv/" + h32 + ".narinfo", "/priv/nar/" + h32 + ".nar"} {
		resp, _ := http.Get(ts.URL + p)
		if resp.StatusCode != 401 {
			t.Errorf("anon GET %s → %d want 401", p, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

func TestStatusRingTrimAndFutureRange(t *testing.T) {
	s, _, _ := newTestServerCfg(t, nil)
	s.stat.mu.Lock()
	s.stat.pts = make([]statusPoint, ringCap+1)
	s.stat.mu.Unlock()
	s.sampleStatus() // append + trim back to ringCap
	s.stat.mu.Lock()
	n := len(s.stat.pts)
	s.stat.mu.Unlock()
	if n != ringCap {
		t.Fatalf("ring len %d want %d", n, ringCap)
	}
	// live window smaller than the ring → tail slice
	set := s.statusSeries(statusRangeQ{WinMin: 10})
	if len(set.req) == 0 {
		t.Fatal("windowed series empty")
	}
	// custom range reaching into the future clamps maxT to now and patches live
	from := time.Now().Truncate(24 * time.Hour)
	set = s.statusSeries(statusRangeQ{Custom: true, From: from, To: from.AddDate(0, 0, 2)})
	if set.maxT > time.Now().Unix() {
		t.Fatalf("future range not clamped: maxT=%d", set.maxT)
	}
}

func TestPushAPIAnonWhenClosed(t *testing.T) {
	_, db, ts := newTestServerCfg(t, func(cfg *config.Config) { cfg.Security.AllowOpenBootstrap = false })
	db.CreateCache("c", true, 40)
	db.CreateToken("exists", nil, []string{"push"}, 0)
	for _, p := range []string{"/c/api/get-missing-paths", "/c/api/get-missing-chunks"} {
		resp, _ := http.Post(ts.URL+p, "application/json", strings.NewReader(`{"hashes":[]}`))
		if resp.StatusCode != 401 {
			t.Errorf("anon %s → %d want 401", p, resp.StatusCode)
		}
		resp.Body.Close()
	}
	if r := put(t, ts, "/c/api/path", []byte(`{}`), ""); r.StatusCode != 401 {
		t.Errorf("anon put path → %d want 401", r.StatusCode)
	}
}

func TestIndexUnknownSingleSegment(t *testing.T) {
	_, _, ts := newTestServerCfg(t, nil)
	resp, _ := http.Get(ts.URL + "/standalone-nonsense")
	if resp.StatusCode != 404 {
		t.Fatalf("single-segment unknown path → %d want 404", resp.StatusCode)
	}
	resp.Body.Close()
}
