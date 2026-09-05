package store

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"
)

// A cache's footprint counts only chunks in its own backend. The lookup used
// to match on hash alone, so an identical chunk stored in a second backend was
// summed too — inflating the reported size — and, because the chunks table is
// keyed (storage, hash), the unscoped query could not use that key and
// full-scanned the table once per batch.
func TestCacheStatsScopedToItsBackend(t *testing.T) {
	db := openTest(t)
	c, err := db.CreateCache("acct", "one", true, 40)
	if err != nil {
		t.Fatal(err)
	}
	const h = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	// The same chunk hash in this cache's backend and in another one.
	if err := db.PutChunk(DefaultStorage, h, 4096, 1000, "k/"+h, 1); err != nil {
		t.Fatal(err)
	}
	if err := db.PutChunk("other", h, 4096, 7777, "k2/"+h, 1); err != nil {
		t.Fatal(err)
	}
	if err := db.PutPath(c.ID, "00000000000000000000000000000000", &Path{
		StorePath: "/nix/store/00000000000000000000000000000000-p",
		NarHash:   "sha256:x", NarSize: 4096, Chunks: []string{h},
	}); err != nil {
		t.Fatal(err)
	}

	st, err := db.CacheStats(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st.Chunks != 1 || st.PhysicalBytes != 1000 {
		t.Fatalf("stats = %+v, want 1 chunk / 1000 bytes from the default backend only", st)
	}
	if st.Paths != 1 || st.LogicalBytes != 4096 {
		t.Fatalf("stats = %+v", st)
	}
}

// StatsFor must read in constant time from a stored row, recompute the first
// time it is asked, and serve a stale row rather than block once it ages.
func TestStatsForCachesAndRefreshes(t *testing.T) {
	db := openTest(t)
	c, err := db.CreateCache("acct", "one", true, 40)
	if err != nil {
		t.Fatal(err)
	}
	put := func(n int, chunk string) {
		t.Helper()
		if err := db.PutChunk(DefaultStorage, chunk, 4096, 1000, "k/"+chunk, 1); err != nil {
			t.Fatal(err)
		}
		h := fmt.Sprintf("%032d", n)
		if err := db.PutPath(c.ID, h, &Path{
			StorePath: "/nix/store/" + h + "-p", NarHash: "sha256:x", NarSize: 4096,
			Chunks: []string{chunk},
		}); err != nil {
			t.Fatal(err)
		}
	}
	put(1, strings.Repeat("a", 64))

	// First call computes and stores.
	st, err := db.StatsFor(c.ID)
	if err != nil || st.Paths != 1 || st.PhysicalBytes != 1000 {
		t.Fatalf("first StatsFor = %+v %v", st, err)
	}
	if _, _, ok, _ := db.storedStats(c.ID); !ok {
		t.Fatal("no row stored after first call")
	}

	// A second path lands, but the stored row is fresh, so the fast read still
	// reports the old figure while CacheStats sees the truth.
	put(2, strings.Repeat("b", 64))
	if st, _ = db.StatsFor(c.ID); st.Paths != 1 {
		t.Fatalf("fresh row should still read 1 path, got %+v", st)
	}
	if exact, _ := db.CacheStats(c.ID); exact.Paths != 2 {
		t.Fatalf("CacheStats must stay exact, got %+v", exact)
	}

	// Age the row past the TTL: the next read serves it and refreshes behind.
	if err := db.write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE cache_stats SET computed=0 WHERE cache_id=?`, c.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if st, _ = db.StatsFor(c.ID); st.Paths != 1 {
		t.Fatalf("stale read should serve the old row, got %+v", st)
	}
	// The background refresh catches up.
	var refreshed bool
	for range 100 {
		if s, _, ok, _ := db.storedStats(c.ID); ok && s.Paths == 2 {
			refreshed = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !refreshed {
		t.Fatal("background refresh never updated the stored row")
	}
	if st, _ = db.StatsFor(c.ID); st.Paths != 2 || st.PhysicalBytes != 2000 {
		t.Fatalf("after refresh = %+v", st)
	}
}

// A deleted cache must not leave its summary behind.
func TestStatsForDroppedWithCache(t *testing.T) {
	db := openTest(t)
	c, err := db.CreateCache("acct", "gone", true, 40)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.StatsFor(c.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, _ := db.storedStats(c.ID); !ok {
		t.Fatal("expected a stored row")
	}
	if err := db.DeleteCache(c.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, _ := db.storedStats(c.ID); ok {
		t.Fatal("summary outlived its cache")
	}
}
