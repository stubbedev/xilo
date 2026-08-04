package push

// The fake server in push_e2e_test.go pins the client's behaviour; this file
// pins the contract, by running the real push client against the real server
// (internal/server) with only the nix exec boundary faked. It is a test-only
// import: the `noserver` build tag only has to keep cmd/xilo compiling.

import (
	"context"
	"crypto/sha256"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stubbedev/xilo/internal/config"
	"github.com/stubbedev/xilo/internal/narinfo"
	"github.com/stubbedev/xilo/internal/server"
	"github.com/stubbedev/xilo/internal/storage"
	"github.com/stubbedev/xilo/internal/store"
)

// realServer starts a server with local storage and open bootstrap (no tokens),
// plus the chunking params the fake NARs below are sized for.
func realServer(t *testing.T) (*store.DB, *httptest.Server) {
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
	cfg.Chunking.MinSize, cfg.Chunking.AvgSize, cfg.Chunking.MaxSize = 1024, 4096, 16384
	cfg.Chunking.NarThreshold = 2048

	db, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	st, err := storage.New(cfg.Storage)
	if err != nil {
		t.Fatal(err)
	}
	s, err := server.New(cfg, db, map[string]storage.Storage{"default": st})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(func() { ts.Close(); db.Close() })
	return db, ts
}

// narOf makes deterministic bytes and the NAR hash the server will verify them
// against (the server hashes the reassembled chunk stream).
func narOf(size int) ([]byte, string) {
	data := randBytes(size)
	sum := sha256.Sum256(data)
	return data, "sha256:" + narinfo.Base32Encode(sum[:])
}

// freshState points the client state cache at an empty directory, so a cached
// manifest can't stand in for what a test means to observe.
func freshState(t *testing.T) {
	t.Helper()
	t.Setenv("XILO_CACHE_DIR", filepath.Join(t.TempDir(), "state"))
}

func fetch(t *testing.T, url string) (int, []byte) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

// The full round trip against the real server: push with the presence filter in
// play, pull the NAR back byte-for-byte, adopt it into a second cache without a
// second dump, and recover from chunks vanishing underneath a cached manifest.
func TestIntegrationRealServerFastPaths(t *testing.T) {
	filterMinBytes = 1 // exercise the real chunk-filter endpoint
	t.Cleanup(func() { filterMinBytes = 16 << 20 })

	db, ts := realServer(t)
	if _, err := db.CreateCache("default", "one", true, 40); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateCache("default", "two", true, 40); err != nil {
		t.Fatal(err)
	}

	nar, narHash := narOf(256 << 10)
	base := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-big"
	fakeNix(t, pathInfoJSON(t, []pathInfo{{Path: pathBig, NarHash: narHash, NarSize: uint64(len(nar))}}),
		map[string][]byte{base: nar})

	push := func(cache string) {
		t.Helper()
		c := NewClient(ts.URL, "default/"+cache, "", 0)
		c.Quiet = true
		if err := c.Push(context.Background(), []string{pathBig}); err != nil {
			t.Fatalf("push to %s: %v", cache, err)
		}
	}
	storeHash := narinfo.StoreHash(pathBig)
	stateOne := os.Getenv("XILO_CACHE_DIR") // the state dir fakeNix set up

	// 1. First push, then the cache must serve the exact bytes back.
	push("one")
	if got := dumpCounts(t)[base]; got != 1 {
		t.Fatalf("dumps = %d, want 1", got)
	}
	if code, body := fetch(t, ts.URL+"/c/default/one/"+storeHash+".narinfo"); code != 200 {
		t.Fatalf("narinfo: %d %s", code, body)
	}
	code, got := fetch(t, ts.URL+"/c/default/one/nar/"+storeHash+".nar")
	if code != 200 {
		t.Fatalf("nar: %d", code)
	}
	if len(got) != len(nar) || sha256.Sum256(got) != sha256.Sum256(nar) {
		t.Fatalf("NAR came back different: %d bytes, want %d", len(got), len(nar))
	}

	// 2. Pushing the same path again is a no-op: the server reports it present.
	push("one")
	if got := dumpCounts(t)[base]; got != 1 {
		t.Fatalf("dumps = %d after a repeat push, want 1", got)
	}

	// 3. Push to a second cache on the same backend from a client with an empty
	// state cache: no manifest can help, so the only way to avoid a dump is the
	// server adopting the identical path from cache one.
	freshState(t)
	push("two")
	if got := dumpCounts(t)[base]; got != 1 {
		t.Fatalf("dumps = %d, want 1: cache two should have adopted the path", got)
	}
	if _, err := db.GetPath(mustCacheID(t, db, "two"), storeHash); err != nil {
		t.Fatalf("path not registered in cache two: %v", err)
	}
	code, got = fetch(t, ts.URL+"/c/default/two/nar/"+storeHash+".nar")
	if code != 200 || sha256.Sum256(got) != sha256.Sum256(nar) {
		t.Fatalf("adopted NAR from cache two: %d, %d bytes", code, len(got))
	}

	// 4. Drop the path and its chunks. With the manifest cache back in play the
	// client tries put-path first, gets a 409, and must dump again to recover.
	t.Setenv("XILO_CACHE_DIR", stateOne)
	p, err := db.GetPath(mustCacheID(t, db, "one"), storeHash)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.EvictPathsOlderThan(time.Now().Unix() + 3600); err != nil { // every path
		t.Fatal(err)
	}
	if err := db.DeleteChunkRows("default", p.Chunks); err != nil {
		t.Fatal(err)
	}
	push("one")
	if got := dumpCounts(t)[base]; got != 2 {
		t.Fatalf("dumps = %d, want 2 (stale manifest, then a real dump)", got)
	}
	code, got = fetch(t, ts.URL+"/c/default/one/nar/"+storeHash+".nar")
	if code != 200 || sha256.Sum256(got) != sha256.Sum256(nar) {
		t.Fatalf("NAR after recovery: %d, %d bytes (want %d)", code, len(got), len(nar))
	}
}

func mustCacheID(t *testing.T, db *store.DB, name string) int64 {
	t.Helper()
	c, err := db.GetCache("default", name)
	if err != nil {
		t.Fatal(err)
	}
	return c.ID
}

// A private source cache is not adoptable by an anonymous pusher, so the client
// falls back to a real dump and upload — and both caches still serve correctly.
func TestIntegrationNoAdoptionFromPrivateCache(t *testing.T) {
	db, ts := realServer(t)
	if _, err := db.CreateCache("default", "priv", false, 40); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateCache("default", "pub", true, 40); err != nil {
		t.Fatal(err)
	}
	nar, narHash := narOf(128 << 10)
	base := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-big"
	fakeNix(t, pathInfoJSON(t, []pathInfo{{Path: pathBig, NarHash: narHash, NarSize: uint64(len(nar))}}),
		map[string][]byte{base: nar})

	// A token is scoped to one cache, so each push carries its own — and the pub
	// token cannot pull priv, which is exactly what blocks adoption. Each push
	// starts from an empty state cache so a manifest can't hide the difference.
	for _, cache := range []string{"priv", "pub"} {
		freshState(t)
		secret, _, err := db.CreateToken(0, cache+"-push", []string{"default/" + cache}, []string{"push"}, 0)
		if err != nil {
			t.Fatal(err)
		}
		c := NewClient(ts.URL, "default/"+cache, secret, 0)
		c.Quiet = true
		if err := c.Push(context.Background(), []string{pathBig}); err != nil {
			t.Fatalf("push to %s: %v", cache, err)
		}
	}
	// Second push had to dump: adoption from a private cache needs read rights.
	if got := dumpCounts(t)[base]; got != 2 {
		t.Fatalf("dumps = %d, want 2 (no adoption from a private source)", got)
	}
	storeHash := narinfo.StoreHash(pathBig)
	code, got := fetch(t, ts.URL+"/c/default/pub/nar/"+storeHash+".nar")
	if code != 200 || sha256.Sum256(got) != sha256.Sum256(nar) {
		t.Fatalf("NAR from the public cache: %d, %d bytes", code, len(got))
	}
}
