package server

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/stubbedev/xilo/internal/api"
	"github.com/stubbedev/xilo/internal/config"
	"github.com/stubbedev/xilo/internal/narinfo"
	"github.com/stubbedev/xilo/internal/storage"
	"github.com/stubbedev/xilo/internal/store"
)

func newTestServer(t *testing.T, openBootstrap bool) (*Server, *store.DB, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	cfg.DataDir = dir
	cfg.BaseURL = "http://example"
	cfg.Storage.Local.Root = filepath.Join(dir, "storage")
	cfg.Security.AllowOpenBootstrap = openBootstrap

	db, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	st, err := storage.New(cfg.Storage)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(cfg, db, map[string]storage.Storage{"default": st})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(func() { ts.Close(); db.Close() })
	return s, db, ts
}

// fakeNar treats arbitrary bytes as a NAR: single chunk = the bytes, so the
// chunk hash and the narHash share one sha256 digest.
func fakeNar(data []byte) (chunkHash, narHash string, narSize uint64) {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), "sha256:" + narinfo.Base32Encode(sum[:]), uint64(len(data))
}

func put(t *testing.T, ts *httptest.Server, path string, body []byte, token string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPut, ts.URL+path, bytes.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func pushFake(t *testing.T, ts *httptest.Server, cache, storeHash string, data []byte, token string) {
	t.Helper()
	chunkHash, narHash, narSize := fakeNar(data)
	if r := put(t, ts, "/c/default/"+cache+"/api/chunk/"+chunkHash, data, token); r.StatusCode != 200 {
		b, _ := io.ReadAll(r.Body)
		t.Fatalf("put chunk: %d %s", r.StatusCode, b)
	}
	pr := api.PathReq{
		StorePath: "/nix/store/" + storeHash + "-pkg", NarHash: narHash, NarSize: narSize,
		Chunks: []string{chunkHash},
	}
	body, _ := json.Marshal(pr)
	if r := put(t, ts, "/c/default/"+cache+"/api/path", body, token); r.StatusCode != 200 {
		b, _ := io.ReadAll(r.Body)
		t.Fatalf("put path: %d %s", r.StatusCode, b)
	}
}

const h32 = "abcdfghijklmnpqrsvwxyz0123456789"

func TestPushPullSignedFlow(t *testing.T) {
	_, db, ts := newTestServer(t, true)
	if _, err := db.CreateCache("default", "c", true, 40); err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte("nar-content-"), 1000)
	pushFake(t, ts, "c", h32, data, "")

	// narinfo signature must verify with the cache's ed25519 pubkey.
	resp, _ := http.Get(ts.URL + "/c/default/c/" + h32 + ".narinfo")
	if resp.StatusCode != 200 {
		t.Fatalf("narinfo status %d", resp.StatusCode)
	}
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("missing immutable cache-control: %q", cc)
	}
	body, _ := io.ReadAll(resp.Body)
	verifyNarinfoSig(t, db, "c", string(body))

	// identity NAR download → exact bytes + correct Content-Length.
	got := getNar(t, ts, "/c/default/c/nar/"+h32+".nar", "")
	if !bytes.Equal(got, data) {
		t.Fatalf("identity NAR mismatch: %d vs %d bytes", len(got), len(data))
	}

	// zstd transfer → decodes to the same bytes.
	zr := getNar(t, ts, "/c/default/c/nar/"+h32+".nar", "zstd")
	if !bytes.Equal(zr, data) {
		t.Fatalf("zstd NAR mismatch")
	}
}

func getNar(t *testing.T, ts *httptest.Server, path, accept string) []byte {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, ts.URL+path, nil)
	if accept != "" {
		req.Header.Set("Accept-Encoding", accept)
	}
	// Disable the transport's automatic gzip so we control decoding.
	tr := &http.Transport{DisableCompression: true}
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	switch resp.Header.Get("Content-Encoding") {
	case "zstd":
		d, _ := zstd.NewReader(nil)
		out, err := d.DecodeAll(raw, nil)
		if err != nil {
			t.Fatalf("zstd decode: %v", err)
		}
		return out
	case "":
		return raw
	default:
		t.Fatalf("unexpected content-encoding %q", resp.Header.Get("Content-Encoding"))
		return nil
	}
}

// TestStatsAccuracy pins the stats contract: HEAD probes aren't downloads,
// narBytes means uncompressed bytes on every encoding path, and counters
// survive a restart via the settings table.
func TestStatsAccuracy(t *testing.T) {
	s, db, ts := newTestServer(t, true)
	if _, err := db.CreateCache("default", "c", true, 40); err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte("nar-content-"), 1000)
	pushFake(t, ts, "c", h32, data, "")

	req, _ := http.NewRequest(http.MethodHead, ts.URL+"/c/default/c/nar/"+h32+".nar", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("HEAD nar status %d", resp.StatusCode)
	}
	if n := s.metrics.narServed.Load(); n != 0 {
		t.Errorf("HEAD counted as a served NAR: %d", n)
	}

	getNar(t, ts, "/c/default/c/nar/"+h32+".nar", "zstd")
	if n := s.metrics.narBytes.Load(); n != int64(len(data)) {
		t.Errorf("zstd narBytes = %d, want uncompressed %d", n, len(data))
	}
	getNar(t, ts, "/c/default/c/nar/"+h32+".nar", "")
	if n := s.metrics.narBytes.Load(); n != 2*int64(len(data)) {
		t.Errorf("identity narBytes = %d, want %d", n, 2*len(data))
	}
	if n := s.metrics.narServed.Load(); n != 2 {
		t.Errorf("narServed = %d, want 2", n)
	}

	s.persistCounters()
	s2, err := New(s.cfg, db, s.sts)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := s2.metrics.narServed.Load(), s.metrics.narServed.Load(); got != want {
		t.Errorf("narServed after restart = %d, want %d", got, want)
	}
	if s2.stat.lastNar != s2.metrics.narBytes.Load() {
		t.Errorf("sampler baseline %d != restored narBytes %d", s2.stat.lastNar, s2.metrics.narBytes.Load())
	}
}

func verifyNarinfoSig(t *testing.T, db *store.DB, cache, narinfoText string) {
	t.Helper()
	c, err := db.GetCache("default", cache)
	if err != nil {
		t.Fatal(err)
	}
	pub := ed25519.PublicKey(c.PrivKey.Public().(ed25519.PublicKey))
	var storePath, narHash, sig string
	var refs []string
	var narSize uint64
	for _, line := range strings.Split(narinfoText, "\n") {
		k, v, ok := strings.Cut(line, ": ")
		if !ok {
			continue
		}
		switch k {
		case "StorePath":
			storePath = v
		case "NarHash":
			narHash = v
		case "NarSize":
			var n uint64
			for _, ch := range v {
				n = n*10 + uint64(ch-'0')
			}
			narSize = n
		case "References":
			if v != "" {
				for _, r := range strings.Fields(v) {
					refs = append(refs, narinfo.StoreDir+"/"+r)
				}
			}
		case "Sig":
			sig = v
		}
	}
	_, b64, _ := strings.Cut(sig, ":")
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatal(err)
	}
	fp := narinfo.Fingerprint(storePath, narHash, narSize, refs)
	if !ed25519.Verify(pub, []byte(fp), raw) {
		t.Fatalf("narinfo signature failed to verify\nfp=%s", fp)
	}
}

func TestChunkHashMismatchRejected(t *testing.T) {
	_, db, ts := newTestServer(t, true)
	db.CreateCache("default", "c", true, 40)
	// upload body whose hash != the URL hash
	r := put(t, ts, "/c/default/c/api/chunk/"+strings.Repeat("0", 64), []byte("not that content"), "")
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", r.StatusCode)
	}
}

func TestVerifyReassemblyRejectsForgedHash(t *testing.T) {
	_, db, ts := newTestServer(t, true)
	db.CreateCache("default", "c", true, 40)
	data := []byte("real chunk content")
	chunkHash, _, _ := fakeNar(data)
	put(t, ts, "/c/default/c/api/chunk/"+chunkHash, data, "") // upload real chunk

	// Register a path claiming a bogus narHash → proof-of-possession fails.
	pr := api.PathReq{
		StorePath: "/nix/store/" + h32 + "-x", NarHash: "sha256:" + strings.Repeat("0", 52),
		NarSize: uint64(len(data)), Chunks: []string{chunkHash},
	}
	body, _ := json.Marshal(pr)
	r := put(t, ts, "/c/default/c/api/path", body, "")
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("forged narHash should 400, got %d", r.StatusCode)
	}
}

func TestAuthGating(t *testing.T) {
	s, db, ts := newTestServer(t, false) // bootstrap closed
	db.CreateCache("default", "pub", true, 40)
	db.CreateCache("default", "priv", false, 40)
	pushTok, _, _ := db.CreateToken(0, "ci", []string{"default/pub"}, []string{"push"}, 0)
	pullTok, _, _ := db.CreateToken(0, "rd", []string{"default/priv"}, []string{"pull"}, 0)
	_ = s

	data := []byte("some data")
	ch, _, _ := fakeNar(data)

	// push without token → 401 (bootstrap closed, tokens exist)
	if r := put(t, ts, "/c/default/pub/api/chunk/"+ch, data, ""); r.StatusCode != 401 {
		t.Fatalf("anon push want 401, got %d", r.StatusCode)
	}
	// push with push token → ok
	if r := put(t, ts, "/c/default/pub/api/chunk/"+ch, data, pushTok); r.StatusCode != 200 {
		t.Fatalf("push-token push want 200, got %d", r.StatusCode)
	}
	// private pull without token → 401
	resp, _ := http.Get(ts.URL + "/c/default/priv/nix-cache-info")
	if resp.StatusCode != 401 {
		t.Fatalf("anon private pull want 401, got %d", resp.StatusCode)
	}
	// private pull with pull token → 200
	req, _ := http.NewRequest("GET", ts.URL+"/c/default/priv/nix-cache-info", nil)
	req.Header.Set("Authorization", "Bearer "+pullTok)
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != 200 {
		t.Fatalf("pull-token private pull want 200, got %d", resp.StatusCode)
	}
	// public pull open
	resp, _ = http.Get(ts.URL + "/c/default/pub/nix-cache-info")
	if resp.StatusCode != 200 {
		t.Fatalf("public pull want 200, got %d", resp.StatusCode)
	}
}

func TestRoutingAndHealth(t *testing.T) {
	_, db, ts := newTestServer(t, true)
	db.CreateCache("default", "c", true, 40)
	cases := []struct {
		path string
		code int
	}{
		{"/healthz", 200},
		{"/metrics", 401}, // requires an admin bearer token

		{"/c/default/c/nix-cache-info", 200},
		{"/c/default/nope/nix-cache-info", 404},
		{"/c/default/c/deadbeef.narinfo", 404}, // valid cache, missing path
		{"/c/notasuffix", 404},
	}
	for _, tc := range cases {
		resp, err := http.Get(ts.URL + tc.path)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != tc.code {
			t.Errorf("%s → %d, want %d", tc.path, resp.StatusCode, tc.code)
		}
	}
}

func TestExtractToken(t *testing.T) {
	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte("user:sekret"))
	cases := []struct{ hdr, want string }{
		{"Bearer abc", "abc"},
		{basic, "sekret"},
		{"Basic !!!notbase64", ""},
		{"", ""},
		{"Weird xyz", ""},
	}
	for _, c := range cases {
		r, _ := http.NewRequest("GET", "/", nil)
		if c.hdr != "" {
			r.Header.Set("Authorization", c.hdr)
		}
		if got := extractToken(r); got != c.want {
			t.Errorf("extractToken(%q)=%q want %q", c.hdr, got, c.want)
		}
	}
}

func TestNegotiateEncoding(t *testing.T) {
	cases := []struct{ in, want string }{
		{"zstd,gzip", "zstd"},
		{"gzip", "gzip"},
		{"gzip, deflate", "gzip"},
		{"", ""},
		{"br", ""},
		{"zstd;q=0, gzip", "gzip"}, // zstd explicitly refused
		{"gzip;q=0", ""},           // gzip refused
		{"identity, zstd;q=0", ""}, // both refused/unsupported
		{"zstd;q=0.5, gzip;q=0.1", "zstd"},
	}
	for _, c := range cases {
		if got := negotiateEncoding(c.in); got != c.want {
			t.Errorf("negotiateEncoding(%q)=%q want %q", c.in, got, c.want)
		}
	}
}
