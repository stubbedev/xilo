package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stubbedev/xilo/internal/storage"
)

func openTest(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// The whole reason the project exists: concurrent writes must serialize with no
// SQLITE_BUSY and no lost rows. Run with -race.
func TestSingleWriterConcurrency(t *testing.T) {
	db := openTest(t)
	const goroutines, per = 40, 100
	var wg sync.WaitGroup
	errc := make(chan error, goroutines*per)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < per; i++ {
				h := fmt.Sprintf("chunk-%03d-%03d", g, i)
				if err := db.PutChunk(h, 10, 5, "k/"+h, 1); err != nil {
					errc <- err
				}
			}
		}(g)
	}
	wg.Wait()
	close(errc)
	for err := range errc {
		t.Fatalf("concurrent write failed: %v", err)
	}
	all, err := db.AllChunks()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != goroutines*per {
		t.Fatalf("got %d chunks, want %d", len(all), goroutines*per)
	}
}

// A write fn returning an error must not wedge the writer goroutine.
func TestWriterSurvivesError(t *testing.T) {
	db := openTest(t)
	err := db.write(func(tx *sql.Tx) error { return errors.New("boom") })
	if err == nil || err.Error() != "boom" {
		t.Fatalf("want boom, got %v", err)
	}
	// The next write must still succeed.
	if _, err := db.CreateCache("default", "after", true, 40); err != nil {
		t.Fatalf("writer wedged: %v", err)
	}
}

// A panic inside a write fn must be recovered, not wedge the writer.
func TestWriterSurvivesPanic(t *testing.T) {
	db := openTest(t)
	err := db.write(func(tx *sql.Tx) error { panic("kaboom") })
	if err == nil {
		t.Fatal("expected error from panicking write")
	}
	if _, err := db.CreateCache("default", "after-panic", true, 40); err != nil {
		t.Fatalf("writer wedged after panic: %v", err)
	}
}

func TestAuthorizeMatrix(t *testing.T) {
	db := openTest(t)
	scoped, _, _ := mustToken(t, db, "scoped", []string{"default/a"}, []string{"pull"}, 0)
	pushonly, _, _ := mustToken(t, db, "pushonly", nil, []string{"push"}, 0)
	both, _, _ := mustToken(t, db, "both", []string{"default/a", "default/b"}, []string{"push", "pull"}, 0)
	expired, _, _ := mustToken(t, db, "expired", nil, []string{"pull"}, 1) // expires at unix 1
	revoked, rt, _ := mustToken(t, db, "revoked", nil, []string{"pull"}, 0)
	if err := db.RevokeToken(rt.ID); err != nil {
		t.Fatal(err)
	}

	now := int64(1000)
	cases := []struct {
		secret, cache, perm string
		want                bool
	}{
		{scoped, "a", "pull", true},
		{scoped, "b", "pull", false}, // out of scope
		{scoped, "a", "push", false}, // wrong perm
		{pushonly, "anything", "push", true},
		{pushonly, "anything", "pull", false},
		{both, "a", "push", true},
		{both, "b", "pull", true},
		{both, "c", "pull", false},
		{expired, "a", "pull", false}, // now(1000) >= expires(1)
		{revoked, "a", "pull", false},
		{"garbage-secret", "a", "pull", false},
	}
	for _, c := range cases {
		if got := db.Authorize(c.secret, "default", c.cache, c.perm, now); got != c.want {
			t.Errorf("Authorize(%s,%s,%s)=%v want %v", c.secret[:6], c.cache, c.perm, got, c.want)
		}
	}
}

func mustToken(t *testing.T, db *DB, name string, caches, perms []string, expires int64) (string, *Token, error) {
	t.Helper()
	s, tok, err := db.CreateToken(0, name, caches, perms, expires)
	if err != nil {
		t.Fatal(err)
	}
	return s, tok, err
}

func TestMissingPathsAndChunksDedup(t *testing.T) {
	db := openTest(t)
	c, _ := db.CreateCache("default", "c", true, 40)
	other, _ := db.CreateCache("default", "o", true, 40)
	putPath(t, db, c.ID, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", nil)
	putPath(t, db, other.ID, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", nil)

	// duplicate input hashes → deduped output; cross-cache scoping
	miss, err := db.MissingPaths(c.ID, []string{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "zzz", "zzz", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"})
	if err != nil {
		t.Fatal(err)
	}
	// "a" present in c; "b" present only in other cache → missing for c; "zzz" missing once
	want := map[string]bool{"zzz": true, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb": true}
	if len(miss) != 2 {
		t.Fatalf("missing=%v", miss)
	}
	for _, m := range miss {
		if !want[m] {
			t.Fatalf("unexpected missing %q", m)
		}
	}

	db.PutChunk("c1", 1, 1, "k", 1)
	mc, _ := db.MissingChunks([]string{"c1", "c2", "c2"})
	if len(mc) != 1 || mc[0] != "c2" {
		t.Fatalf("MissingChunks=%v", mc)
	}
}

func TestPutPathUpsertRoundTrip(t *testing.T) {
	db := openTest(t)
	c, _ := db.CreateCache("default", "c", true, 40)
	h := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	putPath(t, db, c.ID, h, []string{"x", "y"})
	// upsert with new chunks
	p := &Path{StorePath: "/nix/store/" + h + "-n", NarHash: "sha256:z", NarSize: 5, Refs: []string{"/nix/store/r"}, Chunks: []string{"z1"}}
	if err := db.PutPath(c.ID, h, p); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetPath(c.ID, h)
	if err != nil {
		t.Fatal(err)
	}
	if got.NarSize != 5 || len(got.Chunks) != 1 || got.Chunks[0] != "z1" || len(got.Refs) != 1 {
		t.Fatalf("upsert not applied: %+v", got)
	}
	// still exactly one row (present, not duplicated)
	miss, _ := db.MissingPaths(c.ID, []string{h})
	if len(miss) != 0 {
		t.Fatalf("path should be present")
	}
	// empty chunks/refs round-trip to nil, not [""]
	putPath(t, db, c.ID, "cccccccccccccccccccccccccccccccc", nil)
	g2, _ := db.GetPath(c.ID, "cccccccccccccccccccccccccccccccc")
	if g2.Chunks != nil || g2.Refs != nil {
		t.Fatalf("empty should decode to nil, got chunks=%v refs=%v", g2.Chunks, g2.Refs)
	}
}

func putPath(t *testing.T, db *DB, cacheID int64, storeHash string, chunks []string) {
	t.Helper()
	p := &Path{StorePath: "/nix/store/" + storeHash + "-n", NarHash: "sha256:h", NarSize: 1, Chunks: chunks}
	if err := db.PutPath(cacheID, storeHash, p); err != nil {
		t.Fatal(err)
	}
}

func TestGCGraceAndMarkSweep(t *testing.T) {
	db := openTest(t)
	st, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// live chunk (referenced), old orphan, and a fresh orphan inside grace.
	db.PutChunk("live", 100, 50, storage.ChunkKey("live"), 100)
	db.PutChunk("oldorphan", 100, 40, storage.ChunkKey("oldorphan"), 100)
	db.PutChunk("neworphan", 100, 30, storage.ChunkKey("neworphan"), 10_000)
	for _, h := range []string{"live", "oldorphan", "neworphan"} {
		st.Put(ctx, storage.ChunkKey(h), strings.NewReader(h))
	}
	c, _ := db.CreateCache("default", "c", true, 40)
	putPath(t, db, c.ID, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", []string{"live"})

	// graceCutoff = 5000: chunks created >= 5000 are protected (neworphan safe).
	deleted, freed, err := db.GC(ctx, st, 5000)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 || freed != 40 {
		t.Fatalf("GC deleted=%d freed=%d, want 1/40 (only oldorphan)", deleted, freed)
	}
	// live + neworphan survive
	if !db.HasChunk("live") || !db.HasChunk("neworphan") || db.HasChunk("oldorphan") {
		t.Fatalf("wrong chunks survived: live=%v new=%v old=%v",
			db.HasChunk("live"), db.HasChunk("neworphan"), db.HasChunk("oldorphan"))
	}
}

// A push that dedups against an old orphaned chunk re-stamps created
// (TouchChunks); the sweep's transactional re-check must then spare it even
// though the sweep snapshot predates the stamp.
func TestGCSparesTouchedChunk(t *testing.T) {
	db := openTest(t)
	st, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	db.PutChunk("reused", 100, 40, storage.ChunkKey("reused"), 100) // old orphan
	st.Put(ctx, storage.ChunkKey("reused"), strings.NewReader("reused"))

	// Simulate the push side of the race: the server promised presence and
	// re-stamped created after GC would have snapshotted.
	if err := db.TouchChunks([]string{"reused"}, 9_000); err != nil {
		t.Fatal(err)
	}
	// deleteChunkRowIf re-checks created inside the tx — must refuse.
	ok, err := db.deleteChunkRowIf("reused", 5_000)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("deleteChunkRowIf removed a re-stamped chunk")
	}
	if !db.HasChunk("reused") {
		t.Fatal("re-stamped chunk vanished")
	}

	// Full sweep honors the new stamp too.
	deleted, _, err := db.GC(ctx, st, 5_000)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 0 || !db.HasChunk("reused") {
		t.Fatalf("GC swept a re-stamped chunk (deleted=%d)", deleted)
	}
}

// A re-upload of an existing chunk must also restart its grace window.
func TestPutChunkRestampsCreated(t *testing.T) {
	db := openTest(t)
	db.PutChunk("c", 100, 40, "k", 100)
	db.PutChunk("c", 100, 40, "k", 9_000) // re-upload
	ok, err := db.deleteChunkRowIf("c", 5_000)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("re-uploaded chunk was sweepable under old stamp")
	}
}

func TestCacheStatsScoped(t *testing.T) {
	db := openTest(t)
	c, _ := db.CreateCache("default", "c", true, 40)
	db.PutChunk("s1", 100, 30, "k1", 1)
	db.PutChunk("s2", 100, 20, "k2", 1)
	p := &Path{StorePath: "/nix/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-n", NarHash: "sha256:h", NarSize: 200, Chunks: []string{"s1", "s2"}}
	db.PutPath(c.ID, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", p)

	st, err := db.CacheStats(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st.Paths != 1 || st.LogicalBytes != 200 || st.Chunks != 2 || st.PhysicalBytes != 50 {
		t.Fatalf("stats=%+v want paths1 logical200 chunks2 physical50", st)
	}
}

func TestTouchPath(t *testing.T) {
	db := openTest(t)
	c, _ := db.CreateCache("default", "c", true, 40)
	h := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	db.PutPath(c.ID, h, &Path{StorePath: "/nix/store/" + h + "-n", NarHash: "sha256:h", NarSize: 1})
	// accessed was ~now; TouchPath with a huge minAge should NOT write.
	db.TouchPath(c.ID, h, 2_000_000_000, 1<<62)
	// TouchPath far in the future with small minAge SHOULD bump.
	db.TouchPath(c.ID, h, 2_000_000_000, 1)
	var accessed int64
	db.r.QueryRow(`SELECT accessed FROM paths WHERE cache_id=? AND store_hash=?`, c.ID, h).Scan(&accessed)
	if accessed != 2_000_000_000 {
		t.Fatalf("accessed=%d, want bumped to 2e9", accessed)
	}
}

func TestSearchPathsFuzzy(t *testing.T) {
	db := openTest(t)
	c, _ := db.CreateCache("default", "c", true, 40)
	put := func(hash32, name string) {
		p := &Path{StorePath: "/nix/store/" + hash32 + "-" + name, NarHash: "sha256:h", NarSize: 1}
		if err := db.PutPath(c.ID, hash32, p); err != nil {
			t.Fatal(err)
		}
	}
	put("11111111111111111111111111111111", "firefox-128.0")
	put("22222222222222222222222222222222", "ripgrep-14.1.0")
	put("33333333333333333333333333333333", "hello-2.12")

	// exact substring
	got, total, err := db.SearchPaths(c.ID, "firefox", 10, 0, "", "")
	if err != nil || total != 1 || len(got) != 1 {
		t.Fatalf("substring: got %d/%d err=%v", len(got), total, err)
	}
	// fuzzy subsequence: r-p-g-p hits ripgrep only
	got, total, _ = db.SearchPaths(c.ID, "rpgrp", 10, 0, "", "")
	if total != 1 || len(got) != 1 || !strings.Contains(got[0].StorePath, "ripgrep") {
		t.Fatalf("fuzzy: got %v total %d", got, total)
	}
	// multi-term AND
	_, total, _ = db.SearchPaths(c.ID, "fire 128", 10, 0, "", "")
	if total != 1 {
		t.Fatalf("multi-term: total %d", total)
	}
	// case-insensitive
	_, total, _ = db.SearchPaths(c.ID, "FIREFOX", 10, 0, "", "")
	if total != 1 {
		t.Fatalf("case: total %d", total)
	}
	// LIKE wildcards in the term are literal, not wildcards
	_, total, _ = db.SearchPaths(c.ID, "%", 10, 0, "", "")
	if total != 0 {
		t.Fatalf("escape: %% matched %d paths", total)
	}
	// no query = everything
	_, total, _ = db.SearchPaths(c.ID, "", 10, 0, "", "")
	if total != 3 {
		t.Fatalf("empty q: total %d", total)
	}
	// explicit column sort
	got, _, _ = db.SearchPaths(c.ID, "", 10, 0, "path", "asc")
	if len(got) != 3 || !strings.Contains(got[0].StorePath, "firefox") {
		t.Fatalf("sort path asc: %v", got)
	}
}
