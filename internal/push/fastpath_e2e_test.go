package push

// Tests for the three push fast paths: the cached chunk manifest (no dump), the
// server's chunk presence filter (no negotiation round trips), and the
// pipelined windows that keep a dump from stalling on a round trip. Plus the
// backstop that makes optimism safe: put-path's 409 and the pessimistic retry.

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stubbedev/xilo/internal/api"
	"github.com/stubbedev/xilo/internal/bloom"
	"github.com/stubbedev/xilo/internal/chunk"
	"github.com/stubbedev/xilo/internal/narinfo"
)

// saveFilterAged stores a filter and backdates its mtime, so loadFilter treats
// the copy as stale and revalidates.
func saveFilterAged(t *testing.T, key, etag string, body []byte, age time.Duration) {
	t.Helper()
	saveFilter(key, etag, body)
	old := time.Now().Add(-age)
	if err := os.Chtimes(filepath.Join(stateDir(), "filters", key), old, old); err != nil {
		t.Fatal(err)
	}
}

// presentPathsHandler answers get-missing-paths with "nothing missing" (what the
// server does once it has adopted the path), delegating the rest.
func presentPathsHandler(f *fakeServer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/config"):
			json.NewEncoder(w).Encode(f.cfg)
		case strings.HasSuffix(r.URL.Path, "get-missing-paths"):
			json.NewEncoder(w).Encode(api.MissingResp{Missing: nil})
		default:
			f.handle(w, r)
		}
	}
}

func testParams() chunk.Params {
	cfg := baseCfg()
	return chunk.Params{MinSize: cfg.MinSize, AvgSize: cfg.AvgSize, MaxSize: cfg.MaxSize}
}

// chunkList is the chunking of nar under the test params, in NAR order.
func chunkList(t *testing.T, nar []byte) []string {
	t.Helper()
	var out []string
	if err := chunk.SplitHashes(strings.NewReader(string(nar)), testParams(), func(h string) error {
		out = append(out, h)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(out) < 2 {
		t.Fatalf("test NAR produced %d chunks; need >= 2", len(out))
	}
	return out
}

// bigNar sets up a chunked (above-threshold) NAR for pathBig.
func bigNar(t *testing.T, size int) []byte {
	t.Helper()
	nar := randBytes(size)
	fakeNix(t, pathInfoJSON(t, []pathInfo{{Path: pathBig, NarHash: "sha256:y", NarSize: uint64(len(nar))}}),
		map[string][]byte{"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-big": nar})
	return nar
}

func filterOver(t *testing.T, hashes []string) ([]byte, string) {
	t.Helper()
	f := bloom.New(max(len(hashes), 1))
	for _, h := range hashes {
		if err := f.Add(h); err != nil {
			t.Fatal(err)
		}
	}
	return f.Marshal(), `"test-etag"`
}

func filterCfg() api.ConfigResp {
	cfg := baseCfg()
	cfg.ChunkFilter = true
	return cfg
}

// ---- manifest cache (no second dump) ----

func TestManifestCacheSkipsSecondDump(t *testing.T) {
	nar := bigNar(t, 8<<10)
	want := chunkList(t, nar)
	f := newFakeServer(t, baseCfg())

	if err := newTestClient(f, "", 0).Push(context.Background(), []string{pathBig}); err != nil {
		t.Fatal(err)
	}
	if got := dumpCounts(t)["bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-big"]; got != 1 {
		t.Fatalf("first push dumped %d times, want 1", got)
	}

	// Second push: the server still reports the path missing (a fresh cache, a
	// second cache, a GC'd path) but now holds every chunk. The manifest lets
	// the client register it without dumping at all.
	f.haveChunks = map[string]bool{}
	for _, h := range want {
		f.haveChunks[h] = true
	}
	uploadedBefore := len(f.chunks)
	if err := newTestClient(f, "", 0).Push(context.Background(), []string{pathBig}); err != nil {
		t.Fatal(err)
	}
	if got := dumpCounts(t)["bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-big"]; got != 1 {
		t.Fatalf("second push dumped again (%d total); the manifest should have covered it", got)
	}
	if len(f.chunks) != uploadedBefore {
		t.Fatalf("second push uploaded %d new chunks, want 0", len(f.chunks)-uploadedBefore)
	}
	if len(f.paths) != 2 {
		t.Fatalf("paths registered = %d, want 2", len(f.paths))
	}
	// The manifest must reproduce the exact chunk list, order included.
	got := f.paths[1].Chunks
	if len(got) != len(want) {
		t.Fatalf("manifest chunk count = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("manifest chunk %d = %s, want %s", i, got[i], want[i])
		}
	}
}

// A small (whole-NAR) path caches a manifest too.
func TestManifestCacheWholeNar(t *testing.T) {
	nar := []byte("small nar contents")
	fakeNix(t, pathInfoJSON(t, []pathInfo{{Path: pathSmall, NarHash: "sha256:x", NarSize: uint64(len(nar))}}),
		map[string][]byte{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-small": nar})
	f := newFakeServer(t, baseCfg())
	if err := newTestClient(f, "", 0).Push(context.Background(), []string{pathSmall}); err != nil {
		t.Fatal(err)
	}
	f.haveChunks = map[string]bool{chunk.Hash(nar): true}
	if err := newTestClient(f, "", 0).Push(context.Background(), []string{pathSmall}); err != nil {
		t.Fatal(err)
	}
	if got := dumpCounts(t)["aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-small"]; got != 1 {
		t.Fatalf("dumped %d times, want 1 (second push from the manifest)", got)
	}
	if len(f.paths) != 2 {
		t.Fatalf("paths = %d, want 2", len(f.paths))
	}
}

// A manifest whose chunks are gone (server GC'd them) must not fail the push:
// 409, then a real dump.
func TestManifestConflictFallsBackToDump(t *testing.T) {
	nar := bigNar(t, 8<<10)
	want := chunkList(t, nar)
	f := newFakeServer(t, baseCfg())
	if err := newTestClient(f, "", 0).Push(context.Background(), []string{pathBig}); err != nil {
		t.Fatal(err)
	}

	// Second push: the manifest's put-path 409s (one chunk swept), so the client
	// must dump again and re-upload it.
	f.chunks = map[string][]byte{}
	f.haveChunks = map[string]bool{}
	for _, h := range want[1:] {
		f.haveChunks[h] = true
	}
	f.pathPutConflicts = 1
	f.conflictMissing = []string{want[0]}

	if err := newTestClient(f, "", 0).Push(context.Background(), []string{pathBig}); err != nil {
		t.Fatal(err)
	}
	if got := dumpCounts(t)["bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-big"]; got != 2 {
		t.Fatalf("dumps = %d, want 2 (manifest attempt, then the real dump)", got)
	}
	if _, ok := f.chunks[want[0]]; !ok {
		t.Fatal("the swept chunk was not re-uploaded")
	}
	if len(f.chunks) != 1 {
		t.Fatalf("uploaded %d chunks, want only the missing one", len(f.chunks))
	}
	if len(f.paths) != 2 {
		t.Fatalf("paths = %d, want 2 (the retry registered the path)", len(f.paths))
	}
}

// A non-409 failure on the manifest put-path is a real error and must not be
// masked by a silent re-dump.
func TestManifestPutPathHardFailurePropagates(t *testing.T) {
	bigNar(t, 8<<10)
	f := newFakeServer(t, baseCfg())
	if err := newTestClient(f, "", 0).Push(context.Background(), []string{pathBig}); err != nil {
		t.Fatal(err)
	}
	f.failPathPut = true
	err := newTestClient(f, "", 0).Push(context.Background(), []string{pathBig})
	if err == nil {
		t.Fatal("expected the put-path failure to surface")
	}
	if got := dumpCounts(t)["bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-big"]; got != 1 {
		t.Fatalf("dumps = %d, want 1: a 500 must not trigger the pessimistic re-dump", got)
	}
}

// Manifests are namespaced by chunking params: a server that chunks differently
// derives a different chunk list, so the cached one must not be reused.
func TestManifestNotReusedAcrossChunkingParams(t *testing.T) {
	bigNar(t, 8<<10)
	f := newFakeServer(t, baseCfg())
	if err := newTestClient(f, "", 0).Push(context.Background(), []string{pathBig}); err != nil {
		t.Fatal(err)
	}

	cfg := baseCfg()
	cfg.MinSize, cfg.AvgSize, cfg.MaxSize = 128, 512, 2048
	f2 := newFakeServer(t, cfg)
	if err := newTestClient(f2, "", 0).Push(context.Background(), []string{pathBig}); err != nil {
		t.Fatal(err)
	}
	if got := dumpCounts(t)["bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-big"]; got != 2 {
		t.Fatalf("dumps = %d, want 2: different params must re-chunk", got)
	}
}

// ---- presence filter (no negotiation round trips) ----

func TestFilterSkipsNegotiation(t *testing.T) {
	filterMinBytes = 1
	t.Cleanup(func() { filterMinBytes = 16 << 20 })

	nar := bigNar(t, 16<<10)
	want := chunkList(t, nar)
	f := newFakeServer(t, filterCfg())
	// Server holds everything but the first chunk, and says so via the filter.
	f.filter, f.filterETag = filterOver(t, want[1:])
	f.haveChunks = map[string]bool{}
	for _, h := range want[1:] {
		f.haveChunks[h] = true
	}

	if err := newTestClient(f, "", 0).Push(context.Background(), []string{pathBig}); err != nil {
		t.Fatal(err)
	}
	if len(f.chunksAsked) != 0 {
		t.Fatalf("get-missing-chunks called %d times; the filter should have answered locally", len(f.chunksAsked))
	}
	if f.filterAsked != 1 {
		t.Fatalf("chunk-filter fetched %d times, want 1", f.filterAsked)
	}
	if len(f.chunks) != 1 {
		t.Fatalf("uploaded %d chunks, want exactly the one the filter lacked", len(f.chunks))
	}
	if _, ok := f.chunks[want[0]]; !ok {
		t.Fatalf("uploaded the wrong chunk: %v", f.chunks)
	}
	if len(f.paths) != 1 || len(f.paths[0].Chunks) != len(want) {
		t.Fatalf("path registration wrong: %+v", f.paths)
	}
}

// A false positive (or a chunk swept mid-push) must not lose data: put-path
// 409s and the path is redone with the filter off.
func TestFilterFalsePositiveRetriesPessimistically(t *testing.T) {
	filterMinBytes = 1
	t.Cleanup(func() { filterMinBytes = 16 << 20 })

	nar := bigNar(t, 16<<10)
	want := chunkList(t, nar)
	f := newFakeServer(t, filterCfg())
	// The filter claims every chunk, but the server actually holds none.
	f.filter, f.filterETag = filterOver(t, want)
	f.haveChunks = map[string]bool{}
	f.pathPutConflicts = 1
	f.conflictMissing = want

	if err := newTestClient(f, "", 0).Push(context.Background(), []string{pathBig}); err != nil {
		t.Fatal(err)
	}
	if got := dumpCounts(t)["bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-big"]; got != 2 {
		t.Fatalf("dumps = %d, want 2 (optimistic pass, then the pessimistic retry)", got)
	}
	if len(f.chunksAsked) == 0 {
		t.Fatal("the retry must negotiate: it may not trust the filter again")
	}
	if len(f.chunks) != len(want) {
		t.Fatalf("uploaded %d chunks, want all %d", len(f.chunks), len(want))
	}
	for i, h := range want {
		if _, ok := f.chunks[h]; !ok {
			t.Fatalf("chunk %d (%s) never made it", i, h)
		}
	}
	if len(f.paths) != 1 {
		t.Fatalf("paths = %d, want 1 (registered after the retry)", len(f.paths))
	}
}

// The filter is cached on disk: a second push inside the freshness window makes
// no request at all.
func TestFilterCachedBetweenPushes(t *testing.T) {
	filterMinBytes = 1
	t.Cleanup(func() { filterMinBytes = 16 << 20 })

	nar := bigNar(t, 16<<10)
	want := chunkList(t, nar)
	f := newFakeServer(t, filterCfg())
	f.filter, f.filterETag = filterOver(t, nil) // empty: everything gets uploaded
	if err := newTestClient(f, "", 0).Push(context.Background(), []string{pathBig}); err != nil {
		t.Fatal(err)
	}
	if f.filterAsked != 1 {
		t.Fatalf("first push fetched the filter %d times, want 1", f.filterAsked)
	}

	// Force a re-dump path (server forgot the path) and confirm no second fetch.
	f.haveChunks = map[string]bool{}
	for _, h := range want {
		f.haveChunks[h] = true
	}
	if err := newTestClient(f, "", 0).Push(context.Background(), []string{pathBig}); err != nil {
		t.Fatal(err)
	}
	if f.filterAsked != 1 {
		t.Fatalf("chunk-filter fetched %d times; a fresh on-disk copy must be reused without a request", f.filterAsked)
	}
}

// Stale on-disk copy: the client revalidates with If-None-Match and keeps using
// its copy on 304.
func TestFilterRevalidatesWhenStale(t *testing.T) {
	filterMinBytes = 1
	t.Cleanup(func() { filterMinBytes = 16 << 20 })

	nar := bigNar(t, 16<<10)
	want := chunkList(t, nar)
	f := newFakeServer(t, filterCfg())
	f.filter, f.filterETag = filterOver(t, want)
	c := newTestClient(f, "", 0)
	c.loadFilter(context.Background())
	if c.filter == nil {
		t.Fatal("filter not loaded")
	}

	// Age the cached copy past the freshness window.
	key := filterKey(c.base, c.cache)
	body, etag, _ := cachedFilter(key)
	if body == nil || etag != f.filterETag {
		t.Fatalf("filter not cached on disk (etag %q)", etag)
	}
	saveFilterAged(t, key, etag, body, filterFresh+time.Minute)

	c2 := newTestClient(f, "", 0)
	c2.loadFilter(context.Background())
	if c2.filter == nil {
		t.Fatal("stale copy not reused after 304")
	}
	if f.filterAsked != 2 {
		t.Fatalf("filter requests = %d, want 2 (the stale copy revalidates)", f.filterAsked)
	}
	if len(f.filterEtags) < 2 || f.filterEtags[1] != f.filterETag {
		t.Fatalf("revalidation did not send If-None-Match: %v", f.filterEtags)
	}
	if !c2.filter.Has(want[0]) {
		t.Fatal("revalidated filter lost its contents")
	}
	// And the 304 refreshed the local copy, so the next push skips the request.
	c3 := newTestClient(f, "", 0)
	c3.loadFilter(context.Background())
	if f.filterAsked != 2 {
		t.Fatalf("filter requests = %d; the 304 should have marked the copy fresh", f.filterAsked)
	}
}

func TestFilterAbsentFallsBackToNegotiation(t *testing.T) {
	filterMinBytes = 1
	t.Cleanup(func() { filterMinBytes = 16 << 20 })

	nar := bigNar(t, 16<<10)
	want := chunkList(t, nar)
	f := newFakeServer(t, filterCfg())
	f.filter = nil // server answers 204: too many chunks to filter

	if err := newTestClient(f, "", 0).Push(context.Background(), []string{pathBig}); err != nil {
		t.Fatal(err)
	}
	if len(f.chunksAsked) == 0 {
		t.Fatal("without a filter the client must negotiate")
	}
	if len(f.chunks) != len(want) {
		t.Fatalf("uploaded %d chunks, want %d", len(f.chunks), len(want))
	}
}

// An old server advertises no filter: the client must not even try the endpoint.
func TestFilterNotFetchedWhenServerLacksIt(t *testing.T) {
	filterMinBytes = 1
	t.Cleanup(func() { filterMinBytes = 16 << 20 })

	bigNar(t, 16<<10)
	f := newFakeServer(t, baseCfg()) // ChunkFilter false
	if err := newTestClient(f, "", 0).Push(context.Background(), []string{pathBig}); err != nil {
		t.Fatal(err)
	}
	if f.filterAsked != 0 {
		t.Fatalf("chunk-filter requested %d times against a server that doesn't advertise it", f.filterAsked)
	}
}

// Small pushes skip the download: the filter would cost more than it saves.
func TestFilterSkippedForSmallPush(t *testing.T) {
	bigNar(t, 16<<10) // well under the default filterMinBytes
	f := newFakeServer(t, filterCfg())
	f.filter, f.filterETag = filterOver(t, nil)
	if err := newTestClient(f, "", 0).Push(context.Background(), []string{pathBig}); err != nil {
		t.Fatal(err)
	}
	if f.filterAsked != 0 {
		t.Fatalf("fetched the filter for a %d-byte push", 16<<10)
	}
}

// A corrupt filter body is ignored rather than trusted or fatal.
func TestFilterCorruptBodyIgnored(t *testing.T) {
	filterMinBytes = 1
	t.Cleanup(func() { filterMinBytes = 16 << 20 })

	nar := bigNar(t, 16<<10)
	want := chunkList(t, nar)
	f := newFakeServer(t, filterCfg())
	f.filter, f.filterETag = []byte("this is not a filter"), `"junk"`

	if err := newTestClient(f, "", 0).Push(context.Background(), []string{pathBig}); err != nil {
		t.Fatal(err)
	}
	if len(f.chunksAsked) == 0 {
		t.Fatal("corrupt filter must fall back to negotiation")
	}
	if len(f.chunks) != len(want) {
		t.Fatalf("uploaded %d chunks, want %d", len(f.chunks), len(want))
	}
}

// If even the pessimistic retry 409s (chunks disappearing as fast as they're
// uploaded), the push must fail loudly rather than leave a half-registered path.
func TestRepeatedConflictFailsLoudly(t *testing.T) {
	bigNar(t, 8<<10)
	f := newFakeServer(t, baseCfg())
	f.pathPutConflicts = 10
	f.conflictMissing = []string{hex64(1), hex64(2)}

	err := newTestClient(f, "", 0).Push(context.Background(), []string{pathBig})
	if err == nil {
		t.Fatal("expected the push to fail")
	}
	if !strings.Contains(err.Error(), pathBig) || !strings.Contains(err.Error(), "2 referenced chunks are not on the server") {
		t.Fatalf("err = %v, want the path and the chunk count", err)
	}
	if len(f.paths) != 0 {
		t.Fatal("a path was registered despite every put-path conflicting")
	}
	if got := dumpCounts(t)["bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-big"]; got != 2 {
		t.Fatalf("dumps = %d, want 2: exactly one pessimistic retry, no loop", got)
	}
}

// ---- pipelining ----

// The dump must not block on a negotiation round trip. The server holds the
// first get-missing-chunks until a second one arrives: that can only happen if
// the dump kept going while the first was in flight.
func TestWindowNegotiationIsPipelined(t *testing.T) {
	nar := bigNar(t, 512<<10) // >= 2 windows at 32 chunks of ~256B
	want := chunkList(t, nar)
	if len(want) < 2*missingWindow {
		t.Fatalf("test NAR produced %d chunks; need >= %d for two windows", len(want), 2*missingWindow)
	}
	f := newFakeServer(t, baseCfg())
	f.missingChunksBarrier = newBarrier(2)

	// jobs=1: even with a single upload slot the negotiation must overlap.
	if err := newTestClient(f, "", 1).Push(context.Background(), []string{pathBig}); err != nil {
		t.Fatal(err)
	}
	if !f.missingChunksBarrier.reached() {
		t.Fatal("only one get-missing-chunks was ever in flight: the dump is still stalling on each round trip")
	}
	if f.maxNegotiate < 2 {
		t.Fatalf("peak concurrent negotiations = %d, want >= 2", f.maxNegotiate)
	}
	if len(f.chunks) != len(want) {
		t.Fatalf("uploaded %d chunks, want %d", len(f.chunks), len(want))
	}
	if len(f.paths) != 1 || len(f.paths[0].Chunks) != len(want) {
		t.Fatalf("path registration wrong after pipelined push: %+v", f.paths)
	}
}

// Errors from a pipelined window still fail the push, and no path is registered.
func TestPipelinedNegotiationErrorPropagates(t *testing.T) {
	bigNar(t, 128<<10)
	f := newFakeServer(t, baseCfg())
	f.failMissingChunks = true

	err := newTestClient(f, "", 0).Push(context.Background(), []string{pathBig})
	if err == nil || !strings.Contains(err.Error(), "missing-chunks broken") {
		t.Fatalf("err = %v, want the negotiation failure", err)
	}
	if len(f.paths) != 0 {
		t.Fatal("path registered despite a failed negotiation")
	}
}

// ---- adoption wiring ----

// The client must send each path's NAR hash, which is what lets the server
// adopt a copy instead of making us dump.
func TestPushSendsNarHashesForAdoption(t *testing.T) {
	nar := []byte("small nar contents")
	fakeNix(t, pathInfoJSON(t, []pathInfo{{Path: pathSmall, NarHash: "sha256:thehash", NarSize: uint64(len(nar))}}),
		map[string][]byte{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-small": nar})
	f := newFakeServer(t, baseCfg())
	if err := newTestClient(f, "", 0).Push(context.Background(), []string{pathSmall}); err != nil {
		t.Fatal(err)
	}
	if len(f.pathRefs) != 1 || len(f.pathRefs[0]) != 1 {
		t.Fatalf("path refs = %v, want one", f.pathRefs)
	}
	ref := f.pathRefs[0][0]
	if ref.Hash != narinfo.StoreHash(pathSmall) || ref.NarHash != "sha256:thehash" {
		t.Fatalf("ref = %+v, want the store hash + NAR hash", ref)
	}
	// Hashes must still be sent the old way too, so an old server keeps working.
	if len(f.pathsAsked) != 1 || len(f.pathsAsked[0]) != 1 || f.pathsAsked[0][0] != ref.Hash {
		t.Fatalf("hashes = %v, want the store hash", f.pathsAsked)
	}
}

// A path the server reports present (adopted server-side, or already there) is
// never dumped.
func TestPresentPathNeverDumped(t *testing.T) {
	nar := []byte("small nar contents")
	fakeNix(t, pathInfoJSON(t, []pathInfo{{Path: pathSmall, NarHash: "sha256:x", NarSize: uint64(len(nar))}}),
		map[string][]byte{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-small": nar})
	f := newFakeServer(t, baseCfg())
	f.srv.Config.Handler = presentPathsHandler(f)

	if err := newTestClient(f, "", 0).Push(context.Background(), []string{pathSmall}); err != nil {
		t.Fatal(err)
	}
	if len(f.chunks) != 0 || len(f.paths) != 0 {
		t.Fatal("uploaded something for a path the server already has")
	}
}
