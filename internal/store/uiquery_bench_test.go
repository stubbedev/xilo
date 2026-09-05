package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The dashboard, the cache page and the activities page all run their queries
// on every load. These benchmarks seed a cache far past today's production
// size so a query whose cost grows with the table shows up as one.
//
// Seeding goes straight through the writer in bulk — PutPath per row would
// spend the whole benchmark in transaction overhead rather than measuring it.

const benchChunksPerPath = 5

func benchHash(i int) string {
	s := sha256.Sum256([]byte(strconv.Itoa(i)))
	return hex.EncodeToString(s[:])
}

// seedScale bulk-loads one cache with `paths` paths referencing
// paths*benchChunksPerPath distinct chunks, plus `audits` activity rows.
func seedScale(tb testing.TB, db *DB, paths, audits int) int64 {
	tb.Helper()
	c, err := db.CreateCache("bench", "c", true, 40)
	if err != nil {
		tb.Fatal(err)
	}
	err = db.write(func(tx *sql.Tx) error {
		ch, err := tx.Prepare(`INSERT INTO chunks (storage,hash,size,csize,storage_key,created) VALUES (?,?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer ch.Close()
		pa, err := tx.Prepare(`INSERT INTO paths (cache_id,store_hash,store_path,nar_hash,nar_size,deriver,refs,chunks,accessed) VALUES (?,?,?,?,?,'','',?,?)`)
		if err != nil {
			return err
		}
		defer pa.Close()
		au, err := tx.Prepare(`INSERT INTO audit_log (ts,user_id,actor,method,path,status,ip,user_agent,duration_ms) VALUES (?,1,'admin','POST',?,200,'10.0.0.1','curl',3)`)
		if err != nil {
			return err
		}
		defer au.Close()

		for p := range paths {
			var b strings.Builder
			for j := range benchChunksPerPath {
				h := benchHash(p*benchChunksPerPath + j)
				if _, err := ch.Exec(DefaultStorage, h, 1<<19, 1<<18, "k/"+h, 1); err != nil {
					return err
				}
				if j > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(h)
			}
			sh := benchHash(p)[:32]
			if _, err := pa.Exec(c.ID, sh, "/nix/store/"+sh+"-pkg-"+strconv.Itoa(p),
				"sha256:"+benchHash(p)[:52], int64(benchChunksPerPath)<<19, b.String(), int64(p)); err != nil {
				return err
			}
		}
		for a := range audits {
			if _, err := au.Exec(int64(a), "/admin/caches/"+strconv.Itoa(a)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		tb.Fatal(err)
	}
	return c.ID
}

func openBench(tb testing.TB) *DB {
	tb.Helper()
	db, err := Open(filepath.Join(tb.TempDir(), "x.db"))
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { db.Close() })
	return db
}

// benchScale is the row count each UI query is measured at. 100k paths is
// ~12x today's production cache; 500k chunks, 50k activity rows.
const benchScale = 100_000

func BenchmarkUIQueries(b *testing.B) {
	db := openBench(b)
	id := seedScale(b, db, benchScale, 50_000)

	b.Run("StatsFor", func(b *testing.B) {
		if _, err := db.RefreshStats(id); err != nil {
			b.Fatal(err)
		}
		b.ResetTimer()
		for b.Loop() {
			if _, err := db.StatsFor(id); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("CacheStats", func(b *testing.B) {
		for b.Loop() {
			if _, err := db.CacheStats(id); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("GlobalStats", func(b *testing.B) {
		for b.Loop() {
			if _, err := db.GlobalStats(); err != nil {
				b.Fatal(err)
			}
		}
	})
	// Cache page, first page, default sort (most recently pulled).
	b.Run("SearchPaths/recent", func(b *testing.B) {
		for b.Loop() {
			p, _, err := db.SearchPaths(id, "", 25, 0, "", "desc")
			if err != nil || len(p) != 25 {
				b.Fatalf("%d rows %v", len(p), err)
			}
		}
	})
	b.Run("SearchPaths/size", func(b *testing.B) {
		for b.Loop() {
			if _, _, err := db.SearchPaths(id, "", 25, 0, "size", "desc"); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("SearchPaths/name", func(b *testing.B) {
		for b.Loop() {
			if _, _, err := db.SearchPaths(id, "", 25, 0, "path", "asc"); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("SearchPaths/query", func(b *testing.B) {
		for b.Loop() {
			if _, _, err := db.SearchPaths(id, "pkg-993", 25, 0, "", "desc"); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("Activities", func(b *testing.B) {
		for b.Loop() {
			if _, _, err := db.SearchAudit("", "", "", 25, 0, "ts", "desc"); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("ListAccountTokens", func(b *testing.B) {
		for b.Loop() {
			if _, err := db.ListAccountTokens(1); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("UserAccounts", func(b *testing.B) {
		for b.Loop() {
			if _, err := db.UserAccounts(1); err != nil {
				b.Fatal(err)
			}
		}
	})
}
