package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stubbedev/xilo/internal/api"
	"github.com/stubbedev/xilo/internal/config"
	"github.com/stubbedev/xilo/internal/storage"
	"github.com/stubbedev/xilo/internal/store"
)

// countingStorage records blob reads and existence checks, which is how these
// tests observe that put-path stopped streaming the whole NAR back out.
type countingStorage struct {
	storage.Storage
	gets, has atomic.Int64
}

func (c *countingStorage) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	c.gets.Add(1)
	return c.Storage.Get(ctx, key)
}

func (c *countingStorage) Has(ctx context.Context, key string) (bool, error) {
	c.has.Add(1)
	return c.Storage.Has(ctx, key)
}

// newCountingServer is newTestServerCfg with a storage backend that counts
// reads, and small chunks so a modest NAR spans several of them.
func newCountingServer(t *testing.T) (*Server, *store.DB, *httptest.Server, *countingStorage) {
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

	db, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	base, err := storage.New(cfg.Storage)
	if err != nil {
		t.Fatal(err)
	}
	st := &countingStorage{Storage: base}
	s, err := New(cfg, db, map[string]storage.Storage{"default": st})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(func() { ts.Close(); db.Close() })
	return s, db, ts, st
}

// putChunks uploads data as `parts` separate chunks and returns the chunk list
// plus the NAR hash of the concatenation (what put-path verifies against).
func putChunks(t *testing.T, ts *httptest.Server, cache string, parts [][]byte) (chunks []string, narHash string, narSize uint64) {
	t.Helper()
	var all []byte
	for _, p := range parts {
		h, _, _ := fakeNar(p)
		if r := put(t, ts, "/c/default/"+cache+"/api/chunk/"+h, p, ""); r.StatusCode != 200 {
			b, _ := io.ReadAll(r.Body)
			t.Fatalf("put chunk: %d %s", r.StatusCode, b)
		}
		chunks = append(chunks, h)
		all = append(all, p...)
	}
	_, narHash, narSize = fakeNar(all)
	return chunks, narHash, narSize
}

func putPathReq(t *testing.T, ts *httptest.Server, cache, storeHash string, chunks []string, narHash string, narSize uint64) int {
	t.Helper()
	body, _ := json.Marshal(api.PathReq{
		StorePath: "/nix/store/" + storeHash + "-pkg", NarHash: narHash, NarSize: narSize, Chunks: chunks,
	})
	r := put(t, ts, "/c/default/"+cache+"/api/path", body, "")
	io.Copy(io.Discard, r.Body)
	r.Body.Close()
	return r.StatusCode
}

// The point of the cache: registering the same chunk list again must not read
// the chunks back out of storage. On S3 that re-read is the whole NAR.
func TestVerifyCacheSkipsReassemblyReRead(t *testing.T) {
	s, db, ts, st := newCountingServer(t)
	if _, err := db.CreateCache("default", "one", true, 40); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateCache("default", "two", true, 40); err != nil {
		t.Fatal(err)
	}
	parts := [][]byte{bytes.Repeat([]byte("a"), 4096), bytes.Repeat([]byte("b"), 4096), bytes.Repeat([]byte("c"), 4096)}
	chunks, narHash, narSize := putChunks(t, ts, "one", parts)

	if code := putPathReq(t, ts, "one", h32, chunks, narHash, narSize); code != 200 {
		t.Fatalf("first put-path → %d", code)
	}
	firstGets := st.gets.Load()
	if firstGets < int64(len(chunks)) {
		t.Fatalf("first put-path read %d blobs, want >= %d (it must verify)", firstGets, len(chunks))
	}
	if n := s.metrics.verifySkipped.Load(); n != 0 {
		t.Fatalf("verifySkipped = %d after a cold verify", n)
	}

	// Same path again (an upsert re-push).
	if code := putPathReq(t, ts, "one", h32, chunks, narHash, narSize); code != 200 {
		t.Fatalf("re-push → %d", code)
	}
	if got := st.gets.Load(); got != firstGets {
		t.Fatalf("re-push read %d more blobs; the verified chunk list should be remembered", got-firstGets)
	}
	// Existence still checked — that is what catches a vanished blob.
	if st.has.Load() < int64(len(chunks)) {
		t.Fatalf("existence checks = %d, want >= %d", st.has.Load(), len(chunks))
	}
	if n := s.metrics.verifySkipped.Load(); n != 1 {
		t.Fatalf("verifySkipped = %d, want 1", n)
	}

	// Second cache on the same backend: same chunk list, still no re-read.
	if code := putPathReq(t, ts, "two", h32, chunks, narHash, narSize); code != 200 {
		t.Fatalf("second cache → %d", code)
	}
	if got := st.gets.Load(); got != firstGets {
		t.Fatalf("pushing to a second cache re-read %d blobs", got-firstGets)
	}

	// A different chunk list is a cache miss and must be verified for real.
	other := [][]byte{bytes.Repeat([]byte("z"), 4096)}
	oc, onar, osize := putChunks(t, ts, "one", other)
	if code := putPathReq(t, ts, "one", h32b, oc, onar, osize); code != 200 {
		t.Fatalf("new path → %d", code)
	}
	if st.gets.Load() <= firstGets {
		t.Fatal("an unseen chunk list was not verified")
	}
}

// A cache hit must not paper over lost bytes: the existence check fails, the
// full verification runs, and put-path rejects the path.
func TestVerifyCacheStillCatchesLostBlobs(t *testing.T) {
	s, db, ts, _ := newCountingServer(t)
	if _, err := db.CreateCache("default", "one", true, 40); err != nil {
		t.Fatal(err)
	}
	parts := [][]byte{bytes.Repeat([]byte("a"), 4096), bytes.Repeat([]byte("b"), 4096)}
	chunks, narHash, narSize := putChunks(t, ts, "one", parts)
	if code := putPathReq(t, ts, "one", h32, chunks, narHash, narSize); code != 200 {
		t.Fatalf("first put-path → %d", code)
	}

	// Blobs vanish, DB rows stay (bit rot, a botched restore, an S3 lifecycle
	// rule). The chunk list is in the verify cache from the push above.
	if err := os.RemoveAll(s.cfg.Storage.Local.Root); err != nil {
		t.Fatal(err)
	}
	if code := putPathReq(t, ts, "one", h32b, chunks, narHash, narSize); code != 400 {
		t.Fatalf("put-path over lost blobs → %d, want 400", code)
	}
}

func TestVerifyKeyDistinguishesInputs(t *testing.T) {
	chunks := []string{"aa", "bb"}
	base := verifyKey("default", "sha256:x", chunks)
	if base != verifyKey("default", "sha256:x", chunks) {
		t.Fatal("verifyKey is not stable")
	}
	for name, k := range map[string]string{
		"storage":     verifyKey("other", "sha256:x", chunks),
		"narHash":     verifyKey("default", "sha256:y", chunks),
		"chunk order": verifyKey("default", "sha256:x", []string{"bb", "aa"}),
		"chunk set":   verifyKey("default", "sha256:x", []string{"aa", "bb", "cc"}),
		"empty":       verifyKey("default", "sha256:x", nil),
	} {
		if k == base {
			t.Fatalf("verifyKey collides on %s", name)
		}
	}
}

func TestVerifyCacheLRU(t *testing.T) {
	c := newVerifyCache(2)
	if c.has("a") {
		t.Fatal("cold cache reported a hit")
	}
	c.add("a")
	c.add("b")
	if !c.has("a") || !c.has("b") {
		t.Fatal("entries lost")
	}
	c.add("a") // re-adding must not duplicate
	if c.len() != 2 {
		t.Fatalf("len = %d, want 2", c.len())
	}
	// "a" was just touched, so "b" is the eviction victim.
	c.add("c")
	if c.len() != 2 {
		t.Fatalf("len = %d after eviction, want 2", c.len())
	}
	if !c.has("a") || !c.has("c") {
		t.Fatal("LRU evicted the wrong entry")
	}
	if c.has("b") {
		t.Fatal("cache grew past its cap")
	}
	// A nil cache is inert, not a panic (keeps callers free of nil checks).
	var nilCache *verifyCache
	if nilCache.has("a") {
		t.Fatal("nil cache reported a hit")
	}
	nilCache.add("a")
}

// The reassembly check runs unconditionally in multi-tenant mode, even with
// skip_upload_verify set — the cache must not become a way around that.
func TestVerifyCacheHonorsMultiTenantVerification(t *testing.T) {
	s, db, ts, st := newCountingServer(t)
	s.cfg.MultiTenant = true
	s.cfg.Security.SkipUploadVerify = true
	if _, err := db.CreateCache("default", "one", true, 40); err != nil {
		t.Fatal(err)
	}
	parts := [][]byte{bytes.Repeat([]byte("a"), 4096)}
	chunks, narHash, narSize := putChunks(t, ts, "one", parts)
	if code := putPathReq(t, ts, "one", h32, chunks, narHash, narSize); code != 200 {
		t.Fatalf("put-path → %d", code)
	}
	if st.gets.Load() == 0 {
		t.Fatal("multi-tenant put-path skipped verification entirely")
	}
	// And a wrong NarHash over the same chunks is still refused.
	if code := putPathReq(t, ts, "one", h32b, chunks, fmt.Sprintf("sha256:%s", "1111111111111111111111111111111111111111111111111111"), narSize); code != 400 {
		t.Fatalf("bogus narHash → %d, want 400", code)
	}
}
