package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---- caches CRUD ----

func TestCacheCRUD(t *testing.T) {
	db := openTest(t)

	// private cache exercises b2i(false)
	c, err := db.CreateCache("priv", false, 30)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateCache("priv", true, 40); err == nil {
		t.Fatal("duplicate cache name should error")
	}
	db.CreateCache("pub", true, 40)

	got, err := db.GetCache("priv")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != c.ID || got.Public || got.Priority != 30 || got.PubKey != c.PubKey {
		t.Fatalf("GetCache = %+v", got)
	}
	if _, err := db.GetCache("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetCache(missing) = %v, want ErrNotFound", err)
	}

	list, err := db.ListCaches()
	if err != nil || len(list) != 2 || list[0].Name != "priv" || list[1].Name != "pub" {
		t.Fatalf("ListCaches = %v err=%v, want [priv pub]", list, err)
	}

	if err := db.UpdateCache(c.ID, true, 50, 3600, 1<<20); err != nil {
		t.Fatal(err)
	}
	got, _ = db.GetCache("priv")
	if !got.Public || got.Priority != 50 || got.Retention != 3600 || got.MaxBytes != 1<<20 {
		t.Fatalf("after UpdateCache: %+v", got)
	}

	rotated, err := db.RotateKey(c.ID, "priv")
	if err != nil {
		t.Fatal(err)
	}
	if rotated.PubKey == c.PubKey || len(rotated.PrivKey) == 0 {
		t.Fatal("RotateKey did not change the keypair")
	}

	// DeleteCache cascades to paths
	putPath(t, db, c.ID, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", nil)
	if err := db.DeleteCache(c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetCache("priv"); !errors.Is(err, ErrNotFound) {
		t.Fatal("cache still present after delete")
	}
	if _, err := db.GetPath(c.ID, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"); !errors.Is(err, ErrNotFound) {
		t.Fatal("path row survived cache delete (cascade broken)")
	}
}

// ---- tokens ----

func TestTokenCRUD(t *testing.T) {
	db := openTest(t)

	// nil caches/perms default to */pull
	secret, tok, err := db.CreateToken("t1", nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(tok.Caches) != 1 || tok.Caches[0] != "*" || len(tok.Perms) != 1 || tok.Perms[0] != "pull" {
		t.Fatalf("defaults not applied: %+v", tok)
	}
	if !db.Authorize(secret, "any", "pull", 100) {
		t.Fatal("default token should pull anywhere")
	}

	got, err := db.GetToken(tok.ID)
	if err != nil || got.Name != "t1" || got.Revoked {
		t.Fatalf("GetToken = %+v err=%v", got, err)
	}
	if _, err := db.GetToken(9999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetToken(missing) = %v, want ErrNotFound", err)
	}

	db.CreateToken("t2", []string{"a"}, []string{"push"}, 500)
	list, err := db.ListTokens()
	if err != nil || len(list) != 2 || list[0].Name != "t1" || list[1].Name != "t2" {
		t.Fatalf("ListTokens = %v err=%v", list, err)
	}

	// UpdateToken with explicit values, then with empty slices (defaults again)
	if err := db.UpdateToken(tok.ID, "renamed", []string{"a", "b"}, []string{"push"}, 777); err != nil {
		t.Fatal(err)
	}
	got, _ = db.GetToken(tok.ID)
	if got.Name != "renamed" || len(got.Caches) != 2 || got.Perms[0] != "push" || got.Expires != 777 {
		t.Fatalf("after UpdateToken: %+v", got)
	}
	if err := db.UpdateToken(tok.ID, "renamed", nil, nil, 0); err != nil {
		t.Fatal(err)
	}
	got, _ = db.GetToken(tok.ID)
	if got.Caches[0] != "*" || got.Perms[0] != "pull" || got.Expires != 0 {
		t.Fatalf("UpdateToken defaults: %+v", got)
	}
}

func TestTokenExpired(t *testing.T) {
	cases := []struct {
		expires, now int64
		want         bool
	}{
		{0, 1 << 62, false}, // 0 = never
		{5, 4, false},
		{5, 5, true}, // boundary: now == expires is expired
		{5, 6, true},
	}
	for _, c := range cases {
		tok := Token{Expires: c.expires}
		if got := tok.Expired(c.now); got != c.want {
			t.Errorf("Expired(exp=%d, now=%d) = %v, want %v", c.expires, c.now, got, c.want)
		}
	}
}

// ---- sessions ----

func TestSessionLifecycle(t *testing.T) {
	db := openTest(t)
	future := time.Now().Add(time.Hour)

	// expired row gets pruned by the next CreateSession
	if err := db.CreateSession("stale", time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if db.SessionValid("stale") {
		t.Fatal("expired session should not validate")
	}
	if err := db.CreateSession("live", future); err != nil {
		t.Fatal(err)
	}
	var n int
	db.r.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&n)
	if n != 1 {
		t.Fatalf("stale session not pruned, %d rows", n)
	}
	if !db.SessionValid("live") {
		t.Fatal("live session should validate")
	}
	if db.SessionValid("unknown") {
		t.Fatal("unknown session should not validate")
	}
	if err := db.DropSession("live"); err != nil {
		t.Fatal(err)
	}
	if db.SessionValid("live") {
		t.Fatal("dropped session should not validate")
	}
}

// ---- metrics ----

func TestMetricsAddRangePrune(t *testing.T) {
	db := openTest(t)
	base := time.Now().Unix() / 60 * 60

	// A row older than retention is pruned by the next Add.
	old := MetricMinute{TS: time.Now().Add(-91 * 24 * time.Hour).Unix(), Req: 1}
	if err := db.AddMetricMinute(old); err != nil {
		t.Fatal(err)
	}
	for i := int64(0); i < 3; i++ {
		m := MetricMinute{TS: base + i*60, Req: float64(i), Lat: 0.5, Bps: 100, Stored: 42}
		if err := db.AddMetricMinute(m); err != nil {
			t.Fatal(err)
		}
	}
	// same-ts add replaces, not duplicates
	if err := db.AddMetricMinute(MetricMinute{TS: base, Req: 9}); err != nil {
		t.Fatal(err)
	}

	all, err := db.MetricRange(0, 1<<62)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("got %d rows, want 3 (old pruned, same-ts replaced)", len(all))
	}
	if all[0].Req != 9 {
		t.Fatalf("replace not applied: %+v", all[0])
	}

	// half-open range [from, to)
	got, _ := db.MetricRange(base, base+120)
	if len(got) != 2 || got[0].TS != base || got[1].TS != base+60 {
		t.Fatalf("range: %+v", got)
	}
}

// ---- passkeys ----

func TestPasskeyCRUD(t *testing.T) {
	db := openTest(t)
	if err := db.AddPasskey("yubi", []byte(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := db.AddPasskey("phone", []byte(`{"b":2}`)); err != nil {
		t.Fatal(err)
	}
	list, err := db.ListPasskeys()
	if err != nil || len(list) != 2 || list[0].Name != "yubi" || list[1].Name != "phone" {
		t.Fatalf("ListPasskeys = %v err=%v", list, err)
	}
	if err := db.UpdatePasskeyCredential(list[0].ID, []byte(`{"a":2}`)); err != nil {
		t.Fatal(err)
	}
	list, _ = db.ListPasskeys()
	if string(list[0].Credential) != `{"a":2}` {
		t.Fatalf("credential not updated: %s", list[0].Credential)
	}
	if err := db.DeletePasskey(list[0].ID); err != nil {
		t.Fatal(err)
	}
	list, _ = db.ListPasskeys()
	if len(list) != 1 || list[0].Name != "phone" {
		t.Fatalf("after delete: %v", list)
	}
}

// ---- stats ----

func TestGlobalStats(t *testing.T) {
	db := openTest(t)
	g, err := db.GlobalStats()
	if err != nil || g.Caches != 0 || g.Paths != 0 || g.Chunks != 0 {
		t.Fatalf("empty stats: %+v err=%v", g, err)
	}
	c, _ := db.CreateCache("c", true, 40)
	db.PutChunk("g1", 100, 60, "k1", 1)
	db.PutChunk("g2", 100, 40, "k2", 1)
	db.PutPath(c.ID, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		&Path{StorePath: "/nix/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-n", NarHash: "sha256:h", NarSize: 200, Chunks: []string{"g1"}})
	g, err = db.GlobalStats()
	if err != nil {
		t.Fatal(err)
	}
	if g.Caches != 1 || g.Paths != 1 || g.LogicalBytes != 200 || g.Chunks != 2 || g.StoredBytes != 100 {
		t.Fatalf("GlobalStats = %+v", g)
	}
}

func TestCacheStatsEmpty(t *testing.T) {
	db := openTest(t)
	c, _ := db.CreateCache("c", true, 40)
	st, err := db.CacheStats(c.ID)
	if err != nil || st.Paths != 0 || st.Chunks != 0 || st.LogicalBytes != 0 || st.PhysicalBytes != 0 {
		t.Fatalf("empty cache stats = %+v err=%v", st, err)
	}
}

// ---- admin not-found paths ----

func TestAdminNotFound(t *testing.T) {
	db := openTest(t)
	if _, err := db.AdminPasswordHash(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("AdminPasswordHash = %v, want ErrNotFound", err)
	}
	if _, _, err := db.TOTP(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("TOTP = %v, want ErrNotFound", err)
	}
}

// ---- paths / chunks ----

func TestGetPathNotFound(t *testing.T) {
	db := openTest(t)
	c, _ := db.CreateCache("c", true, 40)
	if _, err := db.GetPath(c.ID, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetPath(missing) = %v, want ErrNotFound", err)
	}
}

func TestChunkKeysOrderAndMissing(t *testing.T) {
	db := openTest(t)
	db.PutChunk("h1", 10, 5, "k1", 1)
	db.PutChunk("h2", 20, 15, "k2", 1)

	refs, err := db.ChunkKeys([]string{"h2", "h1"}) // request order != insert order
	if err != nil {
		t.Fatal(err)
	}
	if refs[0].Hash != "h2" || refs[0].Key != "k2" || refs[0].Size != 20 || refs[0].CSize != 15 ||
		refs[1].Hash != "h1" || refs[1].Key != "k1" {
		t.Fatalf("ChunkKeys order/fields: %+v", refs)
	}
	if _, err := db.ChunkKeys([]string{"h1", "gone"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ChunkKeys(missing) = %v, want ErrNotFound", err)
	}
	// empty input
	refs, err = db.ChunkKeys(nil)
	if err != nil || len(refs) != 0 {
		t.Fatalf("ChunkKeys(nil) = %v err=%v", refs, err)
	}
}

// eachBatch must split queries above batchVars (900) placeholders — a single IN
// list of 1000+ would work today but this pins the batching path.
func TestBatchingOverBatchVars(t *testing.T) {
	db := openTest(t)
	const n = batchVars + 150 // forces two batches
	hashes := make([]string, n)
	for i := range hashes {
		hashes[i] = fmt.Sprintf("h%04d", i)
		if err := db.PutChunk(hashes[i], 1, 1, "k", 1); err != nil {
			t.Fatal(err)
		}
	}
	miss, err := db.MissingChunks(append(hashes, "extra1", "extra2"))
	if err != nil {
		t.Fatal(err)
	}
	if len(miss) != 2 {
		t.Fatalf("missing = %d, want 2", len(miss))
	}
	if err := db.TouchChunks(hashes, 12345); err != nil {
		t.Fatal(err)
	}
	var minCreated int64
	db.r.QueryRow(`SELECT MIN(created) FROM chunks`).Scan(&minCreated)
	if minCreated != 12345 {
		t.Fatalf("TouchChunks batch missed rows: min created=%d", minCreated)
	}
	refs, err := db.ChunkKeys(hashes)
	if err != nil || len(refs) != n {
		t.Fatalf("ChunkKeys over batch: %d err=%v", len(refs), err)
	}
	// MissingPaths batches too (with the extra cache_id arg per batch)
	c, _ := db.CreateCache("c", true, 40)
	miss, err = db.MissingPaths(c.ID, hashes)
	if err != nil || len(miss) != n {
		t.Fatalf("MissingPaths over batch: %d err=%v", len(miss), err)
	}
}

func TestTouchChunksEmpty(t *testing.T) {
	db := openTest(t)
	if err := db.TouchChunks(nil, 1); err != nil {
		t.Fatal(err)
	}
}

func TestHelpers(t *testing.T) {
	if placeholders(0) != "" || placeholders(1) != "?" || placeholders(3) != "?,?,?" {
		t.Fatalf("placeholders: %q %q %q", placeholders(0), placeholders(1), placeholders(3))
	}
	if splitLines("") != nil {
		t.Fatal("splitLines(\"\") should be nil")
	}
	if got := splitLines("a\nb"); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("splitLines: %v", got)
	}
	if got := distinct([]string{"a", "b", "a"}); len(got) != 2 {
		t.Fatalf("distinct: %v", got)
	}
	if got := diff([]string{"a", "b", "a"}, map[string]bool{"b": true}); len(got) != 1 || got[0] != "a" {
		t.Fatalf("diff: %v", got)
	}
}

// ---- search sorts & paging ----

func TestSearchPathsSortAndPaging(t *testing.T) {
	db := openTest(t)
	c, _ := db.CreateCache("c", true, 40)
	put := func(hash32, name string, size uint64, accessed int64) {
		p := &Path{StorePath: "/nix/store/" + hash32 + "-" + name, NarHash: "sha256:h", NarSize: size}
		if err := db.PutPath(c.ID, hash32, p); err != nil {
			t.Fatal(err)
		}
		if err := db.write(func(tx *sql.Tx) error {
			_, err := tx.Exec(`UPDATE paths SET accessed=? WHERE cache_id=? AND store_hash=?`, accessed, c.ID, hash32)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	put("11111111111111111111111111111111", "aaa", 300, 30)
	put("22222222222222222222222222222222", "bbb", 100, 10)
	put("33333333333333333333333333333333", "ccc", 200, 20)

	got, total, err := db.SearchPaths(c.ID, "", 10, 0, "size", "asc")
	if err != nil || total != 3 || got[0].NarSize != 100 || got[2].NarSize != 300 {
		t.Fatalf("size asc: %+v total=%d err=%v", got, total, err)
	}
	got, _, _ = db.SearchPaths(c.ID, "", 10, 0, "size", "desc")
	if got[0].NarSize != 300 {
		t.Fatalf("size desc: %+v", got)
	}
	got, _, _ = db.SearchPaths(c.ID, "", 10, 0, "pulled", "asc")
	if got[0].Accessed != 10 || got[2].Accessed != 30 {
		t.Fatalf("pulled asc: %+v", got)
	}
	// default (no key, no query): accessed DESC
	got, _, _ = db.SearchPaths(c.ID, "", 10, 0, "", "")
	if got[0].Accessed != 30 {
		t.Fatalf("default sort: %+v", got)
	}
	// paging: limit 2 offset 2 → 1 row, total still 3
	got, total, _ = db.SearchPaths(c.ID, "", 2, 2, "path", "asc")
	if total != 3 || len(got) != 1 || !strings.Contains(got[0].StorePath, "ccc") {
		t.Fatalf("paging: %+v total=%d", got, total)
	}
	// no matches
	got, total, _ = db.SearchPaths(c.ID, "zzzzzz", 10, 0, "", "")
	if total != 0 || len(got) != 0 {
		t.Fatalf("no-match: %+v total=%d", got, total)
	}
}

// ---- GC / eviction ----

func TestDeleteChunkRowIfBoundary(t *testing.T) {
	db := openTest(t)
	db.PutChunk("b", 10, 5, "k", 5000)
	// created == cutoff is protected (strict <)
	ok, err := db.deleteChunkRowIf("b", 5000)
	if err != nil || ok {
		t.Fatalf("created==cutoff deleted=%v err=%v, want kept", ok, err)
	}
	ok, err = db.deleteChunkRowIf("b", 5001)
	if err != nil || !ok {
		t.Fatalf("created<cutoff deleted=%v err=%v, want deleted", ok, err)
	}
	// already gone → no rows affected
	ok, _ = db.deleteChunkRowIf("b", 5001)
	if ok {
		t.Fatal("second delete reported rows")
	}
}

func TestEvictPathsOlderThan(t *testing.T) {
	db := openTest(t)
	a, _ := db.CreateCache("a", true, 40)
	b, _ := db.CreateCache("b", true, 40)
	putPathAt(t, db, a.ID, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", nil, 10)
	putPathAt(t, db, a.ID, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", nil, 100)
	putPathAt(t, db, b.ID, "cccccccccccccccccccccccccccccccc", nil, 10)

	// per-cache: only cache a's old row goes; boundary accessed==cutoff survives
	n, err := db.EvictCachePathsOlderThan(a.ID, 100)
	if err != nil || n != 1 {
		t.Fatalf("per-cache evicted %d err=%v, want 1", n, err)
	}
	if m, _ := db.MissingPaths(b.ID, []string{"cccccccccccccccccccccccccccccccc"}); len(m) != 0 {
		t.Fatal("other cache's path evicted")
	}

	// global: remaining old row in b goes, a's accessed=100 survives cutoff=100
	n, err = db.EvictPathsOlderThan(100)
	if err != nil || n != 1 {
		t.Fatalf("global evicted %d err=%v, want 1", n, err)
	}
	if m, _ := db.MissingPaths(a.ID, []string{"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}); len(m) != 0 {
		t.Fatal("accessed==cutoff row should survive (strict <)")
	}
}

func TestEnforceCapNoops(t *testing.T) {
	db := openTest(t)
	c, _ := db.CreateCache("c", true, 40)
	db.PutChunk("x", 100, 100, "k", 1)
	putPathAt(t, db, c.ID, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", []string{"x"}, 10)

	// cap 0 = unlimited
	if n, err := db.EnforceCacheCap(c.ID, 0); err != nil || n != 0 {
		t.Fatalf("cap 0: n=%d err=%v", n, err)
	}
	if n, err := db.EnforceGlobalCap(0); err != nil || n != 0 {
		t.Fatalf("global cap 0: n=%d err=%v", n, err)
	}
	// already under cap → nothing evicted
	if n, err := db.EnforceCacheCap(c.ID, 100); err != nil || n != 0 {
		t.Fatalf("under cap: n=%d err=%v", n, err)
	}
}

// failDelete errors on Delete; other ops unused by GC.
type failDelete struct{}

func (failDelete) Put(context.Context, string, io.Reader) error       { return errors.New("no") }
func (failDelete) Get(context.Context, string) (io.ReadCloser, error) { return nil, errors.New("no") }
func (failDelete) Has(context.Context, string) (bool, error)          { return false, nil }
func (failDelete) Delete(context.Context, string) error               { return errors.New("delete fail") }

func TestGCStorageDeleteError(t *testing.T) {
	db := openTest(t)
	db.PutChunk("orphan", 10, 5, "k", 100)
	deleted, _, err := db.GC(context.Background(), failDelete{}, 5000)
	if err == nil || !strings.Contains(err.Error(), "delete fail") {
		t.Fatalf("GC should surface storage delete error, got %v", err)
	}
	if deleted != 0 {
		t.Fatalf("deleted=%d before the failing blob delete", deleted)
	}
}

// ---- lifecycle: Open errors and use-after-Close ----

func TestOpenBadPath(t *testing.T) {
	if _, err := Open(filepath.Join(t.TempDir(), "missing-dir", "x.db")); err == nil {
		t.Fatal("Open in a missing directory should error")
	}
}

// migrate must surface index-creation failures (name collision with a table).
func TestMigrateIndexError(t *testing.T) {
	raw, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "x.db")+pragmas)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`CREATE TABLE idx_paths_accessed (x)`); err != nil {
		t.Fatal(err)
	}
	if err := migrate(raw); err == nil {
		t.Fatal("migrate should fail when the index name is taken by a table")
	}
}

// After Close, writes degrade to an error and reads fail cleanly — nothing
// panics. This also covers the error branches of the read helpers.
func TestUseAfterClose(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	c, _ := db.CreateCache("c", true, 40)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// writes: closed-channel send recovers into an error
	if err := db.PutChunk("h", 1, 1, "k", 1); err == nil || err.Error() != "store: closed" {
		t.Fatalf("write after close = %v, want store: closed", err)
	}
	if _, err := db.CreateCache("x", true, 40); err == nil {
		t.Fatal("CreateCache after close should error")
	}
	if _, _, err := db.CreateToken("x", nil, nil, 0); err == nil {
		t.Fatal("CreateToken after close should error")
	}
	if _, err := db.EvictPathsOlderThan(1); err == nil {
		t.Fatal("evict after close should error")
	}
	if _, err := db.RotateKey(c.ID, "c"); err == nil {
		t.Fatal("RotateKey after close should error")
	}

	// reads: closed pool returns errors, not panics
	if _, err := db.GetCache("c"); err == nil {
		t.Fatal("GetCache after close should error")
	}
	if _, err := db.ListCaches(); err == nil {
		t.Fatal("ListCaches after close should error")
	}
	if _, err := db.ListTokens(); err == nil {
		t.Fatal("ListTokens after close should error")
	}
	if _, err := db.GetToken(1); err == nil {
		t.Fatal("GetToken after close should error")
	}
	if _, err := db.ListPasskeys(); err == nil {
		t.Fatal("ListPasskeys after close should error")
	}
	if _, err := db.MetricRange(0, 1); err == nil {
		t.Fatal("MetricRange after close should error")
	}
	if _, err := db.GlobalStats(); err == nil {
		t.Fatal("GlobalStats after close should error")
	}
	if _, err := db.CacheStats(c.ID); err == nil {
		t.Fatal("CacheStats after close should error")
	}
	if _, _, err := db.SearchPaths(c.ID, "", 10, 0, "", ""); err == nil {
		t.Fatal("SearchPaths after close should error")
	}
	if _, err := db.GetPath(c.ID, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"); err == nil {
		t.Fatal("GetPath after close should error")
	}
	if _, err := db.MissingChunks([]string{"h"}); err == nil {
		t.Fatal("MissingChunks after close should error")
	}
	if _, err := db.MissingPaths(c.ID, []string{"h"}); err == nil {
		t.Fatal("MissingPaths after close should error")
	}
	if _, err := db.ChunkKeys([]string{"h"}); err == nil {
		t.Fatal("ChunkKeys after close should error")
	}
	if _, err := db.AllChunks(); err == nil {
		t.Fatal("AllChunks after close should error")
	}
	if _, err := db.LiveChunkSet(); err == nil {
		t.Fatal("LiveChunkSet after close should error")
	}
	if _, err := db.chunkSizes(); err == nil {
		t.Fatal("chunkSizes after close should error")
	}
	if _, _, err := db.GC(context.Background(), failDelete{}, 1); err == nil {
		t.Fatal("GC after close should error")
	}
	if _, err := db.EnforceGlobalCap(1); err == nil {
		t.Fatal("EnforceGlobalCap after close should error")
	}
	if db.HasChunk("h") {
		t.Fatal("HasChunk after close should be false")
	}
	if db.SessionValid("s") {
		t.Fatal("SessionValid after close should be false")
	}
	if db.AdminExists() {
		t.Fatal("AdminExists after close should be false")
	}
	db.TouchPath(c.ID, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", 1, 1) // must not panic
}
