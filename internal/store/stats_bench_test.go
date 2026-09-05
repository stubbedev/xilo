package store

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// seedProdShape builds a cache the shape of nix.stubbe.dev: ~8.5k paths whose
// chunk lists reference ~40k distinct chunks. That is the working set the
// dashboard summarises on every page load.
func seedProdShape(tb testing.TB, db *DB, paths, chunksPerPath int) int64 {
	tb.Helper()
	c, err := db.CreateCache("bench", "c", true, 40)
	if err != nil {
		tb.Fatal(err)
	}
	hash := func(i int) string {
		s := sha256.Sum256([]byte(strconv.Itoa(i)))
		return hex.EncodeToString(s[:])
	}
	// Chunks first, in one transaction's worth of batched inserts.
	total := paths * chunksPerPath
	for i := range total {
		if err := db.PutChunk(DefaultStorage, hash(i), 1<<19, 1<<18, "k/"+hash(i), 1); err != nil {
			tb.Fatal(err)
		}
	}
	for p := range paths {
		var b strings.Builder
		for j := range chunksPerPath {
			if j > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(hash(p*chunksPerPath + j))
		}
		sp := fmt.Sprintf("/nix/store/%032d-pkg-%d", p, p)
		if err := db.PutPath(c.ID, hash(p)[:32], &Path{
			StorePath: sp,
			NarHash:   "sha256:" + hash(p)[:52],
			NarSize:   uint64(chunksPerPath) << 19,
			Chunks:    strings.Split(b.String(), "\n"),
		}); err != nil {
			tb.Fatal(err)
		}
	}
	return c.ID
}

// BenchmarkCacheStatsProdShape measures the dashboard's per-cache summary at
// production scale. Run with -benchtime=1x; seeding dominates otherwise.
func BenchmarkCacheStatsProdShape(b *testing.B) {
	db := openBenchDB(b)
	id := seedProdShape(b, db, 8500, 5)
	b.ResetTimer()
	for b.Loop() {
		st, err := db.CacheStats(id)
		if err != nil {
			b.Fatal(err)
		}
		if st.Paths != 8500 {
			b.Fatalf("paths = %d", st.Paths)
		}
	}
}

func BenchmarkGlobalStatsProdShape(b *testing.B) {
	db := openBenchDB(b)
	seedProdShape(b, db, 8500, 5)
	b.ResetTimer()
	for b.Loop() {
		if _, err := db.GlobalStats(); err != nil {
			b.Fatal(err)
		}
	}
}

// openBenchDB is openTest for benchmarks (which take *testing.B).
func openBenchDB(tb testing.TB) *DB {
	tb.Helper()
	db, err := Open(filepath.Join(tb.TempDir(), "x.db"))
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { db.Close() })
	return db
}
