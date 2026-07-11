package store

import (
	"database/sql"
	"testing"
)

func TestEnforceCacheCapLRU(t *testing.T) {
	db := openTest(t)
	c, _ := db.CreateCache("default", "c", true, 40)

	// three chunks, 100 bytes compressed each; three paths each referencing one.
	db.PutChunk("c1", 100, 100, "k1", 1)
	db.PutChunk("c2", 100, 100, "k2", 1)
	db.PutChunk("c3", 100, 100, "k3", 1)
	// distinct accessed times so LRU order is deterministic
	putPathAt(t, db, c.ID, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", []string{"c1"}, 10)
	putPathAt(t, db, c.ID, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", []string{"c2"}, 20)
	putPathAt(t, db, c.ID, "cccccccccccccccccccccccccccccccc", []string{"c3"}, 30)

	// cap at 150 bytes → must evict oldest until distinct chunk size <= 150.
	// 300 total → evict oldest (accessed=10, c1) → 200 → still over → evict
	// next (accessed=20, c2) → 100 <= 150. So 2 evicted, newest (c3) kept.
	n, err := db.EnforceCacheCap(c.ID, 150)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("evicted %d, want 2", n)
	}
	// newest path survives
	miss, _ := db.MissingPaths(c.ID, []string{"cccccccccccccccccccccccccccccccc"})
	if len(miss) != 0 {
		t.Fatal("newest path should survive eviction")
	}
	if m, _ := db.MissingPaths(c.ID, []string{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}); len(m) != 1 {
		t.Fatal("oldest path should be evicted")
	}
}

// A chunk shared by two paths keeps its size until the LAST path is evicted.
func TestEnforceCapSharedChunk(t *testing.T) {
	db := openTest(t)
	c, _ := db.CreateCache("default", "c", true, 40)
	db.PutChunk("shared", 100, 100, "k", 1)
	db.PutChunk("solo", 100, 100, "k2", 1)
	putPathAt(t, db, c.ID, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", []string{"shared"}, 10)
	putPathAt(t, db, c.ID, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", []string{"shared", "solo"}, 20)
	// distinct size = shared(100)+solo(100)=200. Cap 100 → evict oldest (accessed=10),
	// but "shared" still referenced by the newer path → size stays 200-0(shared kept)...
	// evicting path A drops nothing (shared still live). Evict B too → 0. So both go.
	n, err := db.EnforceCacheCap(c.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("evicted %d, want 2 (shared chunk keeps size until last ref gone)", n)
	}
}

func TestEnforceGlobalCap(t *testing.T) {
	db := openTest(t)
	a, _ := db.CreateCache("default", "a", true, 40)
	b, _ := db.CreateCache("default", "b", true, 40)
	db.PutChunk("x", 100, 100, "kx", 1)
	db.PutChunk("y", 100, 100, "ky", 1)
	putPathAt(t, db, a.ID, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", []string{"x"}, 10)
	putPathAt(t, db, b.ID, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", []string{"y"}, 20)
	n, err := db.EnforceGlobalCap(100) // 200 → evict oldest across caches → 100
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("evicted %d, want 1", n)
	}
}

func putPathAt(t *testing.T, db *DB, cacheID int64, storeHash string, chunks []string, accessed int64) {
	t.Helper()
	p := &Path{StorePath: "/nix/store/" + storeHash + "-n", NarHash: "sha256:h", NarSize: 1, Chunks: chunks}
	if err := db.PutPath(cacheID, storeHash, p); err != nil {
		t.Fatal(err)
	}
	// PutPath stamps accessed=now; override for deterministic LRU ordering.
	if err := db.write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE paths SET accessed=? WHERE cache_id=? AND store_hash=?`, accessed, cacheID, storeHash)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
