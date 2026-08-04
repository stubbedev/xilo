package push

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stubbedev/xilo/internal/chunk"
)

const testStoreHash = "abcdfghijklmnpqrsvwxyz0123456789"

func hex64(b byte) string { return strings.Repeat(string("0123456789abcdef"[b%16]), 64) }

func isolateState(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "state")
	t.Setenv("XILO_CACHE_DIR", dir)
	return dir
}

func TestManifestRoundTrip(t *testing.T) {
	isolateState(t)
	p := chunk.Default()
	chunks := []string{hex64(1), hex64(2), hex64(1)} // duplicates are legal in NAR order

	if got := loadManifest(p, testStoreHash, "sha256:abc"); got != nil {
		t.Fatalf("cold cache returned %v", got)
	}
	saveManifest(p, testStoreHash, "sha256:abc", chunks)
	got := loadManifest(p, testStoreHash, "sha256:abc")
	if len(got) != len(chunks) {
		t.Fatalf("got %d hashes, want %d", len(got), len(chunks))
	}
	for i := range chunks {
		if got[i] != chunks[i] {
			t.Fatalf("hash %d = %s, want %s (order must survive)", i, got[i], chunks[i])
		}
	}
}

// The NAR hash is part of the key: an input-addressed path rebuilt
// non-reproducibly keeps its store hash but changes contents, and reusing the
// old chunk list would claim a NAR hash the chunks don't produce.
func TestManifestKeyedByNarHash(t *testing.T) {
	isolateState(t)
	p := chunk.Default()
	saveManifest(p, testStoreHash, "sha256:old", []string{hex64(1)})
	if got := loadManifest(p, testStoreHash, "sha256:new"); got != nil {
		t.Fatalf("manifest reused for different contents: %v", got)
	}
	if got := loadManifest(p, testStoreHash, ""); got != nil {
		t.Fatalf("manifest returned without a NAR hash to check: %v", got)
	}
	if got := loadManifest(p, testStoreHash, "sha256:old"); len(got) != 1 {
		t.Fatalf("matching NAR hash should hit: %v", got)
	}
}

func TestManifestParamsNamespaced(t *testing.T) {
	isolateState(t)
	a := chunk.Params{MinSize: 64, AvgSize: 256, MaxSize: 1024}
	b := chunk.Params{MinSize: 128, AvgSize: 512, MaxSize: 2048}
	saveManifest(a, testStoreHash, "sha256:x", []string{hex64(3)})
	if got := loadManifest(b, testStoreHash, "sha256:x"); got != nil {
		t.Fatalf("manifest leaked across chunking params: %v", got)
	}
	if got := loadManifest(a, testStoreHash, "sha256:x"); len(got) != 1 {
		t.Fatalf("own params lost the manifest: %v", got)
	}
}

func TestManifestRejectsBadInput(t *testing.T) {
	dir := isolateState(t)
	p := chunk.Default()

	// A store hash that isn't one must never become a path component.
	for _, bad := range []string{"", "../escape", "short", strings.Repeat("e", 32), "/abs"} {
		saveManifest(p, bad, "sha256:x", []string{hex64(1)})
		if got := loadManifest(p, bad, "sha256:x"); got != nil {
			t.Fatalf("bad store hash %q accepted: %v", bad, got)
		}
	}
	// Nothing may have been written outside the manifest dir.
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if e.Name() != "manifests" {
				t.Fatalf("unexpected entry in the state dir: %s", e.Name())
			}
		}
	}

	saveManifest(p, testStoreHash, "sha256:x", nil) // empty list is not a manifest
	if got := loadManifest(p, testStoreHash, "sha256:x"); got != nil {
		t.Fatalf("empty manifest stored: %v", got)
	}
	// A NAR hash with a newline would forge extra lines.
	saveManifest(p, testStoreHash, "sha256:x\n"+hex64(9), []string{hex64(1)})
	if got := loadManifest(p, testStoreHash, "sha256:x"); got != nil {
		t.Fatalf("newline in the NAR hash accepted: %v", got)
	}
}

func TestManifestRejectsCorruptFile(t *testing.T) {
	isolateState(t)
	p := chunk.Default()
	dir := manifestDir(p)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"garbage":       "sha256:x\nnot-a-hash\n",
		"truncated":     "sha256:x\n" + hex64(1)[:40],
		"no chunks":     "sha256:x\n",
		"empty":         "",
		"legacy format": hex64(1) + "\n" + hex64(2), // pre-NAR-hash layout
	} {
		if err := os.WriteFile(filepath.Join(dir, testStoreHash), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := loadManifest(p, testStoreHash, "sha256:x"); got != nil {
			t.Fatalf("%s accepted: %v", name, got)
		}
	}
}

func TestManifestNoStateDir(t *testing.T) {
	t.Setenv("XILO_CACHE_DIR", "")
	t.Setenv("HOME", "")
	t.Setenv("XDG_CACHE_HOME", "")
	p := chunk.Default()
	// No usable cache dir: every call is a silent no-op, never a panic.
	saveManifest(p, testStoreHash, "sha256:x", []string{hex64(1)})
	if got := loadManifest(p, testStoreHash, "sha256:x"); got != nil {
		t.Fatalf("got %v with no state dir", got)
	}
	pruneState(p)
}

func TestPruneState(t *testing.T) {
	isolateState(t)
	p := chunk.Default()
	dir := manifestDir(p)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name string, age time.Duration) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("sha256:x\n"+hex64(1)), 0o644); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-age)
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
		return path
	}
	stale := write(testStoreHash, manifestMaxAge+time.Hour)
	fresh := write("0123456789abcdfghijklmnpqrsvwxyz", time.Hour)

	pruneState(p)
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("stale manifest survived the prune")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("fresh manifest was pruned: %v", err)
	}

	// The stamp file must keep a second prune from re-walking the dir, and must
	// not itself be treated as a manifest.
	stale2 := write("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", manifestMaxAge+time.Hour)
	pruneState(p)
	if _, err := os.Stat(stale2); err != nil {
		t.Fatal("prune ran twice in a day")
	}
	if _, err := os.Stat(filepath.Join(dir, ".pruned")); err != nil {
		t.Fatalf("no stamp file: %v", err)
	}
}

func TestFilterCacheRoundTrip(t *testing.T) {
	isolateState(t)
	key := filterKey("http://example", "acme/web")
	if body, etag, _ := cachedFilter(key); body != nil || etag != "" {
		t.Fatal("cold filter cache returned data")
	}
	saveFilter(key, `"tag1"`, []byte("filter-bytes"))
	body, etag, age := cachedFilter(key)
	if string(body) != "filter-bytes" || etag != `"tag1"` {
		t.Fatalf("got %q / %q", body, etag)
	}
	if age > time.Minute {
		t.Fatalf("age = %v, want ~0", age)
	}

	// touchFilter refreshes the copy after a 304.
	old := time.Now().Add(-time.Hour)
	path := filepath.Join(stateDir(), "filters", key)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if _, _, age := cachedFilter(key); age < 30*time.Minute {
		t.Fatalf("backdating did not take: age = %v", age)
	}
	touchFilter(key)
	if _, _, age := cachedFilter(key); age > time.Minute {
		t.Fatalf("touchFilter left age = %v", age)
	}
}

func TestFilterKeyDistinguishesServerAndCache(t *testing.T) {
	a := filterKey("http://one", "acme/web")
	if a != filterKey("http://one", "acme/web") {
		t.Fatal("filterKey is not stable")
	}
	for _, other := range []string{filterKey("http://two", "acme/web"), filterKey("http://one", "acme/other")} {
		if a == other {
			t.Fatal("filterKey collides across servers or caches")
		}
	}
	if strings.ContainsAny(a, "/\\.") {
		t.Fatalf("filterKey %q is not a safe filename", a)
	}
}

func TestParseFilterRejectsJunk(t *testing.T) {
	if f := parseFilter(nil); f != nil {
		t.Fatal("nil body parsed")
	}
	if f := parseFilter([]byte("nope")); f != nil {
		t.Fatal("junk body parsed")
	}
}

func TestWriteFileAtomicOverwrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	writeFileAtomic(path, []byte("one"))
	writeFileAtomic(path, []byte("two"))
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "two" {
		t.Fatalf("got %q, %v", got, err)
	}
	// No temp files left behind.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("%d entries left, want 1: %v", len(entries), entries)
	}
	// An unwritable directory is a silent no-op, not a panic.
	writeFileAtomic(filepath.Join(dir, "missing-subdir", "f"), []byte("x"))
}

func TestValidHash(t *testing.T) {
	if !validHash(testStoreHash) {
		t.Fatal("valid nix base32 store hash rejected")
	}
	for _, bad := range []string{"", "short", strings.Repeat("a", 33), "eeee" + testStoreHash[4:], "ABCD" + testStoreHash[4:], "../" + testStoreHash[3:]} {
		if validHash(bad) {
			t.Fatalf("accepted %q", bad)
		}
	}
	if !validChunkHash(hex64(1)) {
		t.Fatal("valid sha256 hex rejected")
	}
	for _, bad := range []string{"", hex64(1)[:63], hex64(1) + "0", strings.Repeat("g", 64)} {
		if validChunkHash(bad) {
			t.Fatalf("accepted chunk hash %q", bad)
		}
	}
}

// loadFilter must survive a server that isn't there, falling back to whatever
// is on disk rather than failing the push.
func TestLoadFilterOfflineUsesStaleCopy(t *testing.T) {
	isolateState(t)
	f := newFakeServer(t, filterCfg())
	c := newTestClient(f, "", 0)
	body, etag := filterOver(t, []string{hex64(1)})
	saveFilterAged(t, filterKey(c.base, c.cache), etag, body, filterFresh+time.Minute)
	f.srv.Close() // server gone

	c.loadFilter(context.Background())
	if c.filter == nil {
		t.Fatal("unreachable server discarded a usable cached filter")
	}
	if !c.filter.Has(hex64(1)) {
		t.Fatal("cached filter lost its contents")
	}
}
