package server

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stubbedev/xilo/internal/api"
	"github.com/stubbedev/xilo/internal/config"
	"github.com/stubbedev/xilo/internal/narinfo"
	"github.com/stubbedev/xilo/internal/storage"
	"github.com/stubbedev/xilo/internal/store"
)

const h32b = "0123456789abcdfghijklmnpqrsvwxyz"

// newTestServerCfg is newTestServer with a config hook.
func newTestServerCfg(t *testing.T, mut func(*config.Config)) (*Server, *store.DB, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	cfg.DataDir = dir
	cfg.BaseURL = "http://example"
	cfg.Storage.Local.Root = filepath.Join(dir, "storage")
	cfg.Security.AllowOpenBootstrap = true
	if mut != nil {
		mut(cfg)
	}
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

// pushChunked uploads parts as individual chunks then registers the path.
func pushChunked(t *testing.T, ts *httptest.Server, cache, storeHash string, parts [][]byte, refs []string, deriver string) api.PathReq {
	t.Helper()
	h := sha256.New()
	var chunks []string
	var size uint64
	for _, p := range parts {
		sum := sha256.Sum256(p)
		ch := hex.EncodeToString(sum[:])
		chunks = append(chunks, ch)
		h.Write(p)
		size += uint64(len(p))
		if r := put(t, ts, "/c/default/"+cache+"/api/chunk/"+ch, p, ""); r.StatusCode != 200 {
			b, _ := io.ReadAll(r.Body)
			t.Fatalf("put chunk: %d %s", r.StatusCode, b)
		}
	}
	pr := api.PathReq{
		StorePath:  "/nix/store/" + storeHash + "-pkg",
		NarHash:    "sha256:" + narinfo.Base32Encode(h.Sum(nil)),
		NarSize:    size,
		Deriver:    deriver,
		References: refs,
		Chunks:     chunks,
	}
	body, _ := json.Marshal(pr)
	if r := put(t, ts, "/c/default/"+cache+"/api/path", body, ""); r.StatusCode != 200 {
		b, _ := io.ReadAll(r.Body)
		t.Fatalf("put path: %d %s", r.StatusCode, b)
	}
	return pr
}

func rawGet(t *testing.T, ts *httptest.Server, path, accept, token string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, ts.URL+path, nil)
	if accept != "" {
		req.Header.Set("Accept-Encoding", accept)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	tr := &http.Transport{DisableCompression: true}
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestMultiChunkNarEncodings(t *testing.T) {
	_, db, ts := newTestServerCfg(t, nil)
	db.CreateCache("default", "c", true, 40)
	parts := [][]byte{
		bytes.Repeat([]byte("aaaa-part-one-"), 500),
		bytes.Repeat([]byte("bbbb-part-two-"), 700),
		bytes.Repeat([]byte("cccc-part-three-"), 300),
	}
	var want []byte
	for _, p := range parts {
		want = append(want, p...)
	}
	refs := []string{"/nix/store/" + h32b + "-dep"}
	pushChunked(t, ts, "c", h32, parts, refs, "/nix/store/"+h32b+"-pkg.drv")

	// identity: byte-exact with Content-Length.
	resp := rawGet(t, ts, "/c/default/c/nar/"+h32+".nar", "identity", "")
	body, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(body, want) {
		t.Fatalf("identity mismatch: %d vs %d bytes", len(body), len(want))
	}
	if cl := resp.Header.Get("Content-Length"); cl != strconv.Itoa(len(want)) {
		t.Errorf("identity Content-Length %q want %d", cl, len(want))
	}
	if !strings.Contains(resp.Header.Get("Cache-Control"), "immutable") {
		t.Errorf("nar missing immutable cache-control")
	}

	// zstd: stored frames concatenated form a valid multi-frame stream with
	// exact Content-Length.
	resp = rawGet(t, ts, "/c/default/c/nar/"+h32+".nar", "zstd", "")
	raw, _ := io.ReadAll(resp.Body)
	if resp.Header.Get("Content-Encoding") != "zstd" {
		t.Fatalf("want zstd encoding, got %q", resp.Header.Get("Content-Encoding"))
	}
	if cl := resp.Header.Get("Content-Length"); cl != strconv.Itoa(len(raw)) {
		t.Errorf("zstd Content-Length %q want %d", cl, len(raw))
	}
	if got := getNar(t, ts, "/c/default/c/nar/"+h32+".nar", "zstd"); !bytes.Equal(got, want) {
		t.Fatalf("zstd decoded mismatch")
	}

	// gzip: decodes to the same bytes.
	resp = rawGet(t, ts, "/c/default/c/nar/"+h32+".nar", "gzip", "")
	if resp.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("want gzip encoding, got %q", resp.Header.Get("Content-Encoding"))
	}
	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(gz)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("gzip decoded mismatch (err=%v)", err)
	}

	// narinfo carries References, Deriver, and a verifying signature.
	resp, _ = http.Get(ts.URL + "/c/default/c/" + h32 + ".narinfo")
	ni, _ := io.ReadAll(resp.Body)
	text := string(ni)
	if !strings.Contains(text, "References: "+h32b+"-dep") {
		t.Errorf("narinfo missing references:\n%s", text)
	}
	if !strings.Contains(text, "Deriver: "+h32b+"-pkg.drv") {
		t.Errorf("narinfo missing deriver:\n%s", text)
	}
	if !strings.Contains(text, "URL: nar/"+h32+".nar") {
		t.Errorf("narinfo missing URL:\n%s", text)
	}
	verifyNarinfoSig(t, db, "c", text)
}

func TestConfigEndpoint(t *testing.T) {
	_, db, ts := newTestServerCfg(t, func(c *config.Config) { c.Security.AllowOpenBootstrap = false })
	db.CreateCache("default", "pub", true, 40)
	db.CreateCache("default", "priv", false, 40)
	pullTok, _, _ := db.CreateToken(0, "rd", []string{"default/priv"}, []string{"pull"}, 0)
	pushTok, _, _ := db.CreateToken(0, "wr", []string{"default/priv"}, []string{"push"}, 0)

	// public cache: anonymous config is fine and carries the pubkey.
	resp := rawGet(t, ts, "/c/default/pub/api/config", "", "")
	if resp.StatusCode != 200 {
		t.Fatalf("public config: %d", resp.StatusCode)
	}
	var cr api.ConfigResp
	json.NewDecoder(resp.Body).Decode(&cr)
	if cr.PublicKey == "" || !cr.Public || cr.MaxSize == 0 {
		t.Fatalf("bad config resp: %+v", cr)
	}

	cases := []struct {
		token string
		code  int
	}{
		{"", 401},
		{pullTok, 200},
		{pushTok, 200},
		{"bogus", 401},
	}
	for _, c := range cases {
		if resp := rawGet(t, ts, "/c/default/priv/api/config", "", c.token); resp.StatusCode != c.code {
			t.Errorf("priv config token=%q → %d want %d", c.token, resp.StatusCode, c.code)
		}
	}
	if resp := rawGet(t, ts, "/c/default/nope/api/config", "", ""); resp.StatusCode != 404 {
		t.Errorf("unknown cache config → %d want 404", resp.StatusCode)
	}
}

func postJSON(t *testing.T, ts *httptest.Server, path string, v any) *http.Response {
	t.Helper()
	body, _ := json.Marshal(v)
	resp, err := http.Post(ts.URL+path, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestMissingPathsAndChunks(t *testing.T) {
	_, db, ts := newTestServerCfg(t, nil)
	db.CreateCache("default", "c", true, 40)
	data := []byte("chunky content")
	pushFake(t, ts, "c", h32, data, "")
	chunkHash, _, _ := fakeNar(data)

	resp := postJSON(t, ts, "/c/default/c/api/get-missing-paths", api.MissingReq{Hashes: []string{h32, h32b}})
	var mr api.MissingResp
	json.NewDecoder(resp.Body).Decode(&mr)
	if len(mr.Missing) != 1 || mr.Missing[0] != h32b {
		t.Fatalf("missing paths = %v, want [%s]", mr.Missing, h32b)
	}

	absent := strings.Repeat("ab", 32)
	resp = postJSON(t, ts, "/c/default/c/api/get-missing-chunks", api.MissingReq{Hashes: []string{chunkHash, absent}})
	mr = api.MissingResp{}
	json.NewDecoder(resp.Body).Decode(&mr)
	if len(mr.Missing) != 1 || mr.Missing[0] != absent {
		t.Fatalf("missing chunks = %v, want [%s]", mr.Missing, absent)
	}

	// malformed JSON bodies → 400
	for _, p := range []string{"/c/default/c/api/get-missing-paths", "/c/default/c/api/get-missing-chunks"} {
		resp, err := http.Post(ts.URL+p, "application/json", strings.NewReader("{nope"))
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != 400 {
			t.Errorf("%s bad json → %d want 400", p, resp.StatusCode)
		}
	}
}

func TestPutChunkDedupAndMetrics(t *testing.T) {
	_, db, ts := newTestServerCfg(t, nil)
	db.CreateCache("default", "c", true, 40)
	data := []byte("dedup me")
	ch, _, _ := fakeNar(data)
	for i := 0; i < 2; i++ {
		if r := put(t, ts, "/c/default/c/api/chunk/"+ch, data, ""); r.StatusCode != 200 {
			t.Fatalf("upload %d: %d", i, r.StatusCode)
		}
	}
	resp, _ := http.Get(ts.URL + "/metrics")
	body, _ := io.ReadAll(resp.Body)
	text := string(body)
	if !strings.Contains(text, "xilo_chunks_received_total 1") ||
		!strings.Contains(text, "xilo_chunks_deduped_total 1") {
		t.Fatalf("metrics missing dedup counters:\n%s", text)
	}

	// JSON shape
	resp, _ = http.Get(ts.URL + "/metrics?format=json")
	var m map[string]int64
	json.NewDecoder(resp.Body).Decode(&m)
	if m["xilo_chunks_received_total"] != 1 {
		t.Fatalf("json metrics: %v", m)
	}
	if _, ok := m["uptime_seconds"]; !ok {
		t.Fatal("json metrics missing uptime")
	}
}

func TestPutPathErrors(t *testing.T) {
	_, db, ts := newTestServerCfg(t, nil)
	db.CreateCache("default", "c", true, 40)
	data := []byte("actual bytes")
	ch, narHash, narSize := fakeNar(data)
	put(t, ts, "/c/default/c/api/chunk/"+ch, data, "")

	mk := func(pr api.PathReq) int {
		body, _ := json.Marshal(pr)
		return put(t, ts, "/c/default/c/api/path", body, "").StatusCode
	}
	sp := "/nix/store/" + h32 + "-x"

	// unknown chunk referenced → 400
	if c := mk(api.PathReq{StorePath: sp, NarHash: narHash, NarSize: narSize, Chunks: []string{strings.Repeat("cd", 32)}}); c != 400 {
		t.Errorf("missing chunks → %d want 400", c)
	}
	// bad narHash format → 400
	if c := mk(api.PathReq{StorePath: sp, NarHash: "garbage", NarSize: narSize, Chunks: []string{ch}}); c != 400 {
		t.Errorf("bad narHash → %d want 400", c)
	}
	// size mismatch → verification 400
	if c := mk(api.PathReq{StorePath: sp, NarHash: narHash, NarSize: narSize + 1, Chunks: []string{ch}}); c != 400 {
		t.Errorf("size mismatch → %d want 400", c)
	}
	// bad json → 400
	if r := put(t, ts, "/c/default/c/api/path", []byte("{nope"), ""); r.StatusCode != 400 {
		t.Errorf("bad json → %d want 400", r.StatusCode)
	}
	// happy path still works after all that
	if c := mk(api.PathReq{StorePath: sp, NarHash: narHash, NarSize: narSize, Chunks: []string{ch}}); c != 200 {
		t.Errorf("valid path → %d want 200", c)
	}
}

func TestSkipUploadVerify(t *testing.T) {
	_, db, ts := newTestServerCfg(t, func(c *config.Config) { c.Security.SkipUploadVerify = true })
	db.CreateCache("default", "c", true, 40)
	data := []byte("whatever")
	ch, _, _ := fakeNar(data)
	put(t, ts, "/c/default/c/api/chunk/"+ch, data, "")
	// forged-but-well-formed narHash is accepted when verification is off.
	pr := api.PathReq{
		StorePath: "/nix/store/" + h32 + "-x",
		NarHash:   "sha256:" + strings.Repeat("0", 52),
		NarSize:   999, Chunks: []string{ch},
	}
	body, _ := json.Marshal(pr)
	if r := put(t, ts, "/c/default/c/api/path", body, ""); r.StatusCode != 200 {
		t.Fatalf("skip-verify push → %d want 200", r.StatusCode)
	}
}

func TestOversizedChunkRejected(t *testing.T) {
	s, db, ts := newTestServerCfg(t, nil)
	db.CreateCache("default", "c", true, 40)
	big := bytes.Repeat([]byte("x"), int(s.maxChunkBody())+1)
	sum := sha256.Sum256(big)
	// The body is truncated at the cap, so the hash can't match → 400.
	if r := put(t, ts, "/c/default/c/api/chunk/"+hex.EncodeToString(sum[:]), big, ""); r.StatusCode != 400 {
		t.Fatalf("oversized chunk → %d want 400", r.StatusCode)
	}
}

func TestNarAndNarinfo404(t *testing.T) {
	_, db, ts := newTestServerCfg(t, nil)
	db.CreateCache("default", "c", true, 40)

	resp, _ := http.Get(ts.URL + "/c/default/c/nar/" + h32b + ".nar")
	if resp.StatusCode != 404 {
		t.Fatalf("missing nar → %d want 404", resp.StatusCode)
	}
	resp, _ = http.Get(ts.URL + "/c/default/nope/nar/" + h32b + ".nar")
	if resp.StatusCode != 404 {
		t.Fatalf("unknown cache nar → %d want 404", resp.StatusCode)
	}
	resp, _ = http.Get(ts.URL + "/c/default/c/" + h32b + ".narinfo")
	if resp.StatusCode != 404 {
		t.Fatalf("missing narinfo → %d want 404", resp.StatusCode)
	}
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "max-age=30") {
		t.Errorf("negative narinfo cache-control = %q, want max-age=30", cc)
	}
}

func TestTokenLifecycleAuth(t *testing.T) {
	_, db, ts := newTestServerCfg(t, func(c *config.Config) { c.Security.AllowOpenBootstrap = false })
	db.CreateCache("default", "priv", false, 40)

	expired, _, _ := db.CreateToken(0, "old", []string{"default/priv"}, []string{"pull"}, time.Now().Unix()-10)
	if resp := rawGet(t, ts, "/c/default/priv/nix-cache-info", "", expired); resp.StatusCode != 401 {
		t.Errorf("expired token → %d want 401", resp.StatusCode)
	}

	revoked, tok, _ := db.CreateToken(0, "gone", []string{"default/priv"}, []string{"pull"}, 0)
	db.RevokeToken(tok.ID)
	if resp := rawGet(t, ts, "/c/default/priv/nix-cache-info", "", revoked); resp.StatusCode != 401 {
		t.Errorf("revoked token → %d want 401", resp.StatusCode)
	}

	// pull token cannot push
	pullTok, _, _ := db.CreateToken(0, "rd", []string{"default/priv"}, []string{"pull"}, 0)
	data := []byte("d")
	ch, _, _ := fakeNar(data)
	if r := put(t, ts, "/c/default/priv/api/chunk/"+ch, data, pullTok); r.StatusCode != 401 {
		t.Errorf("pull token push → %d want 401", r.StatusCode)
	}

	// Basic auth (netrc-style) carries the pull token
	valid, _, _ := db.CreateToken(0, "net", []string{"default/priv"}, []string{"pull"}, 0)
	req, _ := http.NewRequest("GET", ts.URL+"/c/default/priv/nix-cache-info", nil)
	req.SetBasicAuth("nix", valid)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 200 {
		t.Errorf("basic-auth pull → %d want 200", resp.StatusCode)
	}
	if resp := rawGet(t, ts, "/c/default/priv/nix-cache-info", "", ""); resp.Header.Get("WWW-Authenticate") == "" || resp.StatusCode != 401 {
		t.Errorf("anon private pull should 401 with WWW-Authenticate")
	}
}

func TestBootstrapClosesAfterFirstToken(t *testing.T) {
	_, db, ts := newTestServerCfg(t, nil) // open bootstrap
	db.CreateCache("default", "c", true, 40)
	data := []byte("first")
	ch, _, _ := fakeNar(data)
	if r := put(t, ts, "/c/default/c/api/chunk/"+ch, data, ""); r.StatusCode != 200 {
		t.Fatalf("bootstrap push → %d want 200", r.StatusCode)
	}
	db.CreateToken(0, "first", nil, []string{"push"}, 0)
	data2 := []byte("second")
	ch2, _, _ := fakeNar(data2)
	if r := put(t, ts, "/c/default/c/api/chunk/"+ch2, data2, ""); r.StatusCode != 401 {
		t.Fatalf("post-token anon push → %d want 401", r.StatusCode)
	}
}

func TestHealthJSONAndIndex(t *testing.T) {
	_, _, ts := newTestServerCfg(t, nil)

	resp, _ := http.Get(ts.URL + "/healthz?format=json")
	var h map[string]any
	json.NewDecoder(resp.Body).Decode(&h)
	if h["status"] != "ok" {
		t.Fatalf("healthz json: %v", h)
	}
	req, _ := http.NewRequest("GET", ts.URL+"/healthz", nil)
	req.Header.Set("Accept", "application/json")
	resp, _ = http.DefaultClient.Do(req)
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "json") {
		t.Errorf("accept-negotiated healthz content-type %q", ct)
	}

	// index redirects to /admin
	nr := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, _ = nr.Get(ts.URL + "/")
	if resp.StatusCode != 302 || resp.Header.Get("Location") != "/admin" {
		t.Fatalf("index → %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}

	// negotiated 404: html for browsers, plain otherwise
	req, _ = http.NewRequest("GET", ts.URL+"/c/not-a-narinfo", nil)
	req.Header.Set("Accept", "text/html")
	resp, _ = http.DefaultClient.Do(req)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 404 || !strings.Contains(string(body), "<") {
		t.Errorf("html 404 → %d %q", resp.StatusCode, body)
	}
	resp, _ = http.Get(ts.URL + "/c/not-a-narinfo")
	body, _ = io.ReadAll(resp.Body)
	if resp.StatusCode != 404 || !strings.Contains(string(body), "404 page not found") {
		t.Errorf("plain 404 → %d %q", resp.StatusCode, body)
	}
}

func TestStaticAssets(t *testing.T) {
	_, _, ts := newTestServerCfg(t, nil)
	resp, _ := http.Get(ts.URL + "/static/xilo-tw.css")
	if resp.StatusCode != 200 || resp.Header.Get("ETag") == "" {
		t.Fatalf("static asset: %d etag=%q", resp.StatusCode, resp.Header.Get("ETag"))
	}
	etag := resp.Header.Get("ETag")
	req, _ := http.NewRequest("GET", ts.URL+"/static/xilo-tw.css", nil)
	req.Header.Set("If-None-Match", etag)
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusNotModified {
		t.Fatalf("conditional static → %d want 304", resp.StatusCode)
	}
}
