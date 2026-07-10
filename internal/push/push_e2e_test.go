package push

// End-to-end Push tests: the nix/nix-store exec boundary is crossed via fake
// shell scripts prepended to PATH (no real nix), and the server side is an
// httptest.Server speaking the wire protocol from internal/api.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stubbedev/xilo/internal/api"
	"github.com/stubbedev/xilo/internal/chunk"
	"github.com/stubbedev/xilo/internal/narinfo"
)

// fakeNix installs fake `nix` and `nix-store` executables at the front of PATH.
// `nix path-info` cats the given path-info JSON; `nix-store --dump <path>` cats
// the canned NAR bytes for that path's basename.
func fakeNix(t *testing.T, pathInfoJSON string, nars map[string][]byte) {
	t.Helper()
	dir := t.TempDir()
	writeScript := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "pathinfo.json"), []byte(pathInfoJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	narDir := filepath.Join(dir, "nars")
	if err := os.MkdirAll(narDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for base, data := range nars {
		if err := os.WriteFile(filepath.Join(narDir, base), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeScript("nix", `exec cat "$XILO_TEST_PATHINFO"`)
	writeScript("nix-store", `exec cat "$XILO_TEST_NARDIR/$(basename "$2")"`)
	t.Setenv("XILO_TEST_PATHINFO", filepath.Join(dir, "pathinfo.json"))
	t.Setenv("XILO_TEST_NARDIR", narDir)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// fakeServer implements the push wire protocol and records what was uploaded.
type fakeServer struct {
	t   *testing.T
	cfg api.ConfigResp

	// nil = report everything missing; otherwise return the intersection.
	haveChunks map[string]bool

	failChunkPut bool
	failPathPut  bool

	mu         sync.Mutex
	chunks     map[string][]byte // hash -> uploaded bytes
	paths      []api.PathReq
	auths      map[string]bool // seen Authorization header values
	pathsAsked [][]string      // hashes sent to get-missing-paths

	srv *httptest.Server
}

func newFakeServer(t *testing.T, cfg api.ConfigResp) *fakeServer {
	f := &fakeServer{
		t: t, cfg: cfg,
		chunks: map[string][]byte{},
		auths:  map[string]bool{},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeServer) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.auths[r.Header.Get("Authorization")] = true
	f.mu.Unlock()

	rest, ok := strings.CutPrefix(r.URL.Path, "/c/api/")
	if !ok {
		http.Error(w, "bad path "+r.URL.Path, http.StatusNotFound)
		return
	}
	switch {
	case rest == "config":
		json.NewEncoder(w).Encode(f.cfg)
	case rest == "get-missing-paths" || rest == "get-missing-chunks":
		var req api.MissingReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		missing := req.Hashes
		if rest == "get-missing-paths" {
			f.mu.Lock()
			f.pathsAsked = append(f.pathsAsked, req.Hashes)
			f.mu.Unlock()
		}
		if rest == "get-missing-chunks" && f.haveChunks != nil {
			missing = nil
			for _, h := range req.Hashes {
				if !f.haveChunks[h] {
					missing = append(missing, h)
				}
			}
		}
		json.NewEncoder(w).Encode(api.MissingResp{Missing: missing})
	case strings.HasPrefix(rest, "chunk/"):
		if f.failChunkPut {
			http.Error(w, "chunk store on fire", http.StatusInternalServerError)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		f.mu.Lock()
		f.chunks[strings.TrimPrefix(rest, "chunk/")] = body
		f.mu.Unlock()
	case rest == "path":
		if f.failPathPut {
			http.Error(w, "no", http.StatusInternalServerError)
			return
		}
		var p api.PathReq
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.paths = append(f.paths, p)
		f.mu.Unlock()
	default:
		http.Error(w, "unknown endpoint "+rest, http.StatusNotFound)
	}
}

func baseCfg() api.ConfigResp {
	return api.ConfigResp{
		MinSize: 64, AvgSize: 256, MaxSize: 1024,
		NarThreshold: 512, Parallelism: 4,
	}
}

const (
	pathSmall = "/nix/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-small"
	pathBig   = "/nix/store/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-big"
)

func pathInfoJSON(t *testing.T, infos []pathInfo) string {
	b, err := json.Marshal(infos)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// deterministic pseudo-random bytes so FastCDC finds boundaries
func randBytes(n int) []byte {
	r := rand.New(rand.NewSource(42))
	b := make([]byte, n)
	r.Read(b)
	return b
}

func newTestClient(f *fakeServer, token string, jobs int) *Client {
	c := NewClient(f.srv.URL, "c", token, jobs)
	c.Quiet = true
	return c
}

func TestPushSmallWholeNar(t *testing.T) {
	nar := []byte("small nar contents")
	fakeNix(t, pathInfoJSON(t, []pathInfo{{Path: pathSmall, NarHash: "sha256:x", NarSize: uint64(len(nar))}}),
		map[string][]byte{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-small": nar})
	f := newFakeServer(t, baseCfg())

	c := newTestClient(f, "sekret", 0)
	if err := c.Push(context.Background(), []string{pathSmall}); err != nil {
		t.Fatal(err)
	}

	wantHash := chunk.Hash(nar)
	if got, ok := f.chunks[wantHash]; !ok || string(got) != string(nar) {
		t.Fatalf("chunk not uploaded intact: have %d chunks", len(f.chunks))
	}
	if len(f.paths) != 1 {
		t.Fatalf("paths = %d, want 1", len(f.paths))
	}
	p := f.paths[0]
	if p.StorePath != pathSmall || p.NarHash != "sha256:x" || p.NarSize != uint64(len(nar)) ||
		len(p.Chunks) != 1 || p.Chunks[0] != wantHash {
		t.Fatalf("bad PathReq: %+v", p)
	}
	if !f.auths["Bearer sekret"] {
		t.Fatalf("Authorization header missing: %v", f.auths)
	}
	// hashes asked = store hash of the path
	if len(f.pathsAsked) != 1 || len(f.pathsAsked[0]) != 1 || f.pathsAsked[0][0] != narinfo.StoreHash(pathSmall) {
		t.Fatalf("asked wrong path hashes: %v", f.pathsAsked)
	}
}

func TestPushSmallChunkAlreadyPresent(t *testing.T) {
	nar := []byte("already there")
	fakeNix(t, pathInfoJSON(t, []pathInfo{{Path: pathSmall, NarHash: "sha256:x", NarSize: uint64(len(nar))}}),
		map[string][]byte{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-small": nar})
	f := newFakeServer(t, baseCfg())
	f.haveChunks = map[string]bool{chunk.Hash(nar): true}

	c := newTestClient(f, "", 0)
	if err := c.Push(context.Background(), []string{pathSmall}); err != nil {
		t.Fatal(err)
	}
	if len(f.chunks) != 0 {
		t.Fatalf("uploaded %d chunks, want 0", len(f.chunks))
	}
	if len(f.paths) != 1 {
		t.Fatalf("path not registered")
	}
}

func TestPushChunkedNar(t *testing.T) {
	nar := randBytes(8 << 10) // > NarThreshold(512) -> chunked path
	fakeNix(t, pathInfoJSON(t, []pathInfo{{Path: pathBig, NarHash: "sha256:y", NarSize: uint64(len(nar))}}),
		map[string][]byte{"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-big": nar})
	f := newFakeServer(t, baseCfg())

	// expected chunking, computed independently
	var want []string
	wantData := map[string]string{}
	err := chunk.Split(strings.NewReader(string(nar)), chunk.Params{MinSize: 64, AvgSize: 256, MaxSize: 1024}, func(ch chunk.Chunk) error {
		want = append(want, ch.Hash)
		wantData[ch.Hash] = string(ch.Data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(want) < 2 {
		t.Fatalf("test data produced %d chunks; need >= 2", len(want))
	}

	// server already has the first chunk -> it must NOT be re-uploaded
	f.haveChunks = map[string]bool{want[0]: true}

	c := newTestClient(f, "", 0)
	if err := c.Push(context.Background(), []string{pathBig}); err != nil {
		t.Fatal(err)
	}

	if _, ok := f.chunks[want[0]]; ok {
		t.Fatal("re-uploaded a chunk the server already had")
	}
	if len(f.chunks) != len(want)-1 {
		t.Fatalf("uploaded %d chunks, want %d", len(f.chunks), len(want)-1)
	}
	for _, h := range want[1:] {
		if string(f.chunks[h]) != wantData[h] {
			t.Fatalf("chunk %s bytes corrupted", h)
		}
	}
	if len(f.paths) != 1 {
		t.Fatalf("paths = %d, want 1", len(f.paths))
	}
	p := f.paths[0]
	if len(p.Chunks) != len(want) {
		t.Fatalf("PathReq chunks = %d, want %d (full ordered list)", len(p.Chunks), len(want))
	}
	for i := range want {
		if p.Chunks[i] != want[i] {
			t.Fatalf("chunk order mismatch at %d", i)
		}
	}
}

func TestPushChunkedAllChunksPresent(t *testing.T) {
	nar := randBytes(4 << 10)
	fakeNix(t, pathInfoJSON(t, []pathInfo{{Path: pathBig, NarHash: "sha256:y", NarSize: uint64(len(nar))}}),
		map[string][]byte{"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-big": nar})
	f := newFakeServer(t, baseCfg())
	all := map[string]bool{}
	chunk.SplitHashes(strings.NewReader(string(nar)), chunk.Params{MinSize: 64, AvgSize: 256, MaxSize: 1024}, func(h string) error {
		all[h] = true
		return nil
	})
	f.haveChunks = all

	c := newTestClient(f, "", 0)
	if err := c.Push(context.Background(), []string{pathBig}); err != nil {
		t.Fatal(err)
	}
	if len(f.chunks) != 0 {
		t.Fatalf("uploaded %d chunks, want 0", len(f.chunks))
	}
	if len(f.paths) != 1 || len(f.paths[0].Chunks) != len(all) {
		t.Fatalf("path registration wrong: %+v", f.paths)
	}
}

func TestPushEverythingCached(t *testing.T) {
	fakeNix(t, pathInfoJSON(t, []pathInfo{{Path: pathSmall, NarHash: "sha256:x", NarSize: 1}}), nil)
	f := newFakeServer(t, baseCfg())
	f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/config") {
			json.NewEncoder(w).Encode(baseCfg())
			return
		}
		json.NewEncoder(w).Encode(api.MissingResp{Missing: []string{}}) // nothing missing
	})

	c := newTestClient(f, "", 0)
	if err := c.Push(context.Background(), []string{pathSmall}); err != nil {
		t.Fatal(err)
	}
	if len(f.chunks) != 0 || len(f.paths) != 0 {
		t.Fatal("uploads happened despite everything cached")
	}
}

func TestPushDryRun(t *testing.T) {
	nar := []byte("data")
	fakeNix(t, pathInfoJSON(t, []pathInfo{{Path: pathSmall, NarHash: "sha256:x", NarSize: uint64(len(nar))}}),
		map[string][]byte{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-small": nar})
	f := newFakeServer(t, baseCfg())

	c := newTestClient(f, "", 0)
	c.DryRun = true
	if err := c.Push(context.Background(), []string{pathSmall}); err != nil {
		t.Fatal(err)
	}
	if len(f.chunks) != 0 || len(f.paths) != 0 {
		t.Fatal("dry-run uploaded something")
	}
}

func TestPushUpstreamSignedSkipped(t *testing.T) {
	nar := []byte("mine")
	infos := []pathInfo{
		{Path: pathSmall, NarHash: "sha256:x", NarSize: uint64(len(nar))},
		{Path: pathBig, NarHash: "sha256:y", NarSize: 1, Signatures: []string{"cache.nixos.org-1:zzz"}},
	}
	fakeNix(t, pathInfoJSON(t, infos),
		map[string][]byte{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-small": nar})
	cfg := baseCfg()
	cfg.UpstreamKeys = []string{"cache.nixos.org-1"}
	f := newFakeServer(t, cfg)

	c := newTestClient(f, "", 0)
	if err := c.Push(context.Background(), []string{pathSmall, pathBig}); err != nil {
		t.Fatal(err)
	}
	// only the unsigned path's hash may reach get-missing-paths
	if len(f.pathsAsked) != 1 || len(f.pathsAsked[0]) != 1 || f.pathsAsked[0][0] != narinfo.StoreHash(pathSmall) {
		t.Fatalf("upstream-signed path not skipped: asked %v", f.pathsAsked)
	}
	if len(f.paths) != 1 || f.paths[0].StorePath != pathSmall {
		t.Fatalf("wrong paths registered: %+v", f.paths)
	}
}

func TestPushChunkUploadFailurePropagates(t *testing.T) {
	nar := randBytes(8 << 10)
	fakeNix(t, pathInfoJSON(t, []pathInfo{{Path: pathBig, NarHash: "sha256:y", NarSize: uint64(len(nar))}}),
		map[string][]byte{"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-big": nar})
	f := newFakeServer(t, baseCfg())
	f.failChunkPut = true

	c := newTestClient(f, "", 0)
	err := c.Push(context.Background(), []string{pathBig})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), pathBig) || !strings.Contains(err.Error(), "chunk store on fire") {
		t.Fatalf("error not annotated with path + cause: %v", err)
	}
	if len(f.paths) != 0 {
		t.Fatal("path registered despite failed chunk upload")
	}
}

func TestPushPathPutFailure(t *testing.T) {
	nar := []byte("x")
	fakeNix(t, pathInfoJSON(t, []pathInfo{{Path: pathSmall, NarHash: "sha256:x", NarSize: uint64(len(nar))}}),
		map[string][]byte{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-small": nar})
	f := newFakeServer(t, baseCfg())
	f.failPathPut = true

	c := newTestClient(f, "", 0)
	if err := c.Push(context.Background(), []string{pathSmall}); err == nil {
		t.Fatal("expected error from put path")
	}
}

func TestPushConfigFetchError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "c", "", 0)
	c.Quiet = true
	err := c.Push(context.Background(), []string{pathSmall})
	if err == nil || !strings.Contains(err.Error(), "fetch server config") {
		t.Fatalf("err = %v, want fetch server config", err)
	}
}

func TestPushNixPathInfoError(t *testing.T) {
	// fake nix that fails with stderr, exercising queryClosure + cmdErr
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "nix"),
		[]byte("#!/bin/sh\necho 'no such path' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	f := newFakeServer(t, baseCfg())

	c := newTestClient(f, "", 0)
	err := c.Push(context.Background(), []string{pathSmall})
	if err == nil || !strings.Contains(err.Error(), "nix path-info") || !strings.Contains(err.Error(), "no such path") {
		t.Fatalf("err = %v, want nix path-info with stderr", err)
	}
}

func TestPushMissingEndpointError(t *testing.T) {
	fakeNix(t, pathInfoJSON(t, []pathInfo{{Path: pathSmall, NarHash: "sha256:x", NarSize: 1}}), nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/config") {
			json.NewEncoder(w).Encode(baseCfg())
			return
		}
		http.Error(w, "denied", http.StatusForbidden)
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "c", "", 0)
	c.Quiet = true
	err := c.Push(context.Background(), []string{pathSmall})
	if err == nil || !strings.Contains(err.Error(), "get-missing-paths") {
		t.Fatalf("err = %v, want get-missing-paths error", err)
	}
}

func TestLoadConfigJobs(t *testing.T) {
	cases := []struct {
		name        string
		parallelism int
		override    int
		want        int
	}{
		{"server value used", 8, 0, 8},
		{"override wins", 8, 3, 3},
		{"clamped to 1", 0, 0, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseCfg()
			cfg.Parallelism = tc.parallelism
			f := newFakeServer(t, cfg)
			c := newTestClient(f, "", tc.override)
			params, err := c.loadConfig(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if c.jobs != tc.want {
				t.Fatalf("jobs = %d, want %d", c.jobs, tc.want)
			}
			if params.MinSize != 64 || params.AvgSize != 256 || params.MaxSize != 1024 {
				t.Fatalf("params = %+v", params)
			}
			if c.narThreshold != 512 {
				t.Fatalf("narThreshold = %d", c.narThreshold)
			}
		})
	}
}

func TestLoadConfigBadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "{not json")
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "c", "", 0)
	if _, err := c.loadConfig(context.Background()); err == nil {
		t.Fatal("expected decode error")
	}
}

func TestURLBuilding(t *testing.T) {
	c := NewClient("http://host:1234///", "mycache", "", 0)
	if got := c.url("api", "chunk", "abc"); got != "http://host:1234/mycache/api/chunk/abc" {
		t.Fatalf("url = %q", got)
	}
}

func TestPushDryRunVerbose(t *testing.T) {
	nar := []byte("data")
	fakeNix(t, pathInfoJSON(t, []pathInfo{{Path: pathSmall, NarHash: "sha256:x", NarSize: uint64(len(nar))}}),
		map[string][]byte{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-small": nar})
	f := newFakeServer(t, baseCfg())

	c := newTestClient(f, "", 0)
	c.Quiet = false // exercise logf's printing branch
	c.DryRun = true

	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	pushErr := c.Push(context.Background(), []string{pathSmall})
	w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	if pushErr != nil {
		t.Fatal(pushErr)
	}
	if !strings.Contains(string(out), "dry-run: would push 1/1 paths (4 uncompressed NAR bytes)") {
		t.Fatalf("dry-run output: %q", out)
	}
}

func TestPushWholeDumpError(t *testing.T) {
	// path-info reports the path, but nix-store --dump fails (no canned NAR)
	fakeNix(t, pathInfoJSON(t, []pathInfo{{Path: pathSmall, NarHash: "sha256:x", NarSize: 1}}), nil)
	f := newFakeServer(t, baseCfg())

	c := newTestClient(f, "", 0)
	err := c.Push(context.Background(), []string{pathSmall})
	if err == nil || !strings.Contains(err.Error(), "nix-store --dump") {
		t.Fatalf("err = %v, want nix-store --dump failure", err)
	}
}

func TestPushChunkedDumpError(t *testing.T) {
	// NarSize above threshold forces the chunked path; the dump itself fails.
	fakeNix(t, pathInfoJSON(t, []pathInfo{{Path: pathBig, NarHash: "sha256:y", NarSize: 9999}}), nil)
	f := newFakeServer(t, baseCfg())

	c := newTestClient(f, "", 0)
	err := c.Push(context.Background(), []string{pathBig})
	if err == nil || !strings.Contains(err.Error(), "nix-store --dump") {
		t.Fatalf("err = %v, want nix-store --dump failure", err)
	}
}

func TestMissingBadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "{oops")
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "c", "", 0)
	if _, err := c.missing(context.Background(), "get-missing-paths", []string{"h"}); err == nil {
		t.Fatal("expected decode error")
	}
}

func TestPushNixBinaryNotFound(t *testing.T) {
	// PATH with no nix at all -> exec lookup error (non-ExitError cmdErr branch)
	t.Setenv("PATH", t.TempDir())
	f := newFakeServer(t, baseCfg())
	c := newTestClient(f, "", 0)
	err := c.Push(context.Background(), []string{pathSmall})
	if err == nil || !strings.Contains(err.Error(), "nix path-info") {
		t.Fatalf("err = %v, want nix path-info lookup failure", err)
	}
}

func TestPushWholeChunkPutFailure(t *testing.T) {
	nar := []byte("small")
	fakeNix(t, pathInfoJSON(t, []pathInfo{{Path: pathSmall, NarHash: "sha256:x", NarSize: uint64(len(nar))}}),
		map[string][]byte{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-small": nar})
	f := newFakeServer(t, baseCfg())
	f.failChunkPut = true

	c := newTestClient(f, "", 0)
	if err := c.Push(context.Background(), []string{pathSmall}); err == nil {
		t.Fatal("expected put chunk error")
	}
	if len(f.paths) != 0 {
		t.Fatal("path registered despite failed chunk upload")
	}
}

func TestBadBaseURLRequestErrors(t *testing.T) {
	// a base URL that fails http.NewRequest exercises the request-build error
	// branch in every HTTP helper
	c := NewClient("http://bad url with spaces", "c", "", 0)
	ctx := context.Background()
	if _, err := c.loadConfig(ctx); err == nil {
		t.Fatal("loadConfig should fail")
	}
	if _, err := c.missing(ctx, "get-missing-paths", nil); err == nil {
		t.Fatal("missing should fail")
	}
	if err := c.putChunk(ctx, chunk.Chunk{Hash: "h"}); err == nil {
		t.Fatal("putChunk should fail")
	}
	if err := c.putPath(ctx, api.PathReq{}); err == nil {
		t.Fatal("putPath should fail")
	}
}
