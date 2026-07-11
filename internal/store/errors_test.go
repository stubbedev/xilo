package store

import (
	"context"
	"database/sql"
	"io"
	"path/filepath"
	"testing"
	"time"
)

// exec runs a raw statement through the writer (test-only escape hatch to
// corrupt the schema or plant rows scanners choke on).
func exec(t *testing.T, db *DB, q string, args ...any) {
	t.Helper()
	if err := db.write(func(tx *sql.Tx) error {
		_, err := tx.Exec(q, args...)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// Rows whose INTEGER columns hold text make Scan fail — covers the per-row
// error branches of every list/iterate reader.
func TestScanErrorBranches(t *testing.T) {
	db := openTest(t)
	c, _ := db.CreateCache("c", true, 40)

	exec(t, db, `INSERT INTO caches (name,public,priority,retention,max_bytes,pubkey,privkey,created)
		VALUES ('bad',1,40,0,0,'k',x'00','notanint')`)
	exec(t, db, `INSERT INTO tokens (name,hash,caches,perms,revoked,expires,created)
		VALUES ('bad','h','*','pull',0,0,'notanint')`)
	exec(t, db, `INSERT INTO passkeys (user_id,name,credential,created) VALUES (1,'bad',x'00','notanint')`)
	exec(t, db, `INSERT INTO metrics_minutes (ts,req,lat,bps,stored) VALUES (1,1,1,1,'notanint')`)
	exec(t, db, `INSERT INTO chunks (hash,size,csize,storage_key,created) VALUES ('bad','x','x','k','x')`)
	exec(t, db, `INSERT INTO paths (cache_id,store_hash,store_path,nar_hash,nar_size,accessed)
		VALUES (?, 'badbadbadbadbadbadbadbadbadbadba','/nix/store/x-n','sha256:h','notanint','notanint')`, c.ID)

	if _, err := db.ListCaches(); err == nil {
		t.Error("ListCaches should fail on poison row")
	}
	if _, err := db.GetCache("bad"); err == nil {
		t.Error("GetCache should fail on poison row")
	}
	if _, err := db.ListTokens(); err == nil {
		t.Error("ListTokens should fail on poison row")
	}
	if _, err := db.ListPasskeys(); err == nil {
		t.Error("ListPasskeys should fail on poison row")
	}
	if _, err := db.MetricRange(0, 100); err == nil {
		t.Error("MetricRange should fail on poison row")
	}
	if _, err := db.AllChunks(); err == nil {
		t.Error("AllChunks should fail on poison row")
	}
	if _, err := db.ChunkKeys([]string{"bad"}); err == nil {
		t.Error("ChunkKeys should fail on poison row")
	}
	if _, err := db.chunkSizes(); err == nil {
		t.Error("chunkSizes should fail on poison row")
	}
	if _, _, err := db.SearchPaths(c.ID, "", 10, 0, "", ""); err == nil {
		t.Error("SearchPaths should fail on poison row")
	}
	if _, err := db.CacheStats(c.ID); err == nil {
		t.Error("CacheStats should fail on poison row")
	}
	if _, err := db.EnforceGlobalCap(1); err == nil {
		t.Error("enforceCap should fail scanning poison path")
	}
	if _, err := db.GetPath(c.ID, "badbadbadbadbadbadbadbadbadbadba"); err == nil {
		t.Error("GetPath should fail on poison row")
	}
}

// Dropping tables makes later statements in multi-statement operations fail,
// covering their mid-flight error branches.
func TestDroppedTableErrorBranches(t *testing.T) {
	t.Run("no chunks table", func(t *testing.T) {
		db := openTest(t)
		c, _ := db.CreateCache("c", true, 40)
		putPath(t, db, c.ID, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", []string{"x"})
		exec(t, db, `DROP TABLE chunks`)

		// GlobalStats: caches + paths queries succeed, chunks query fails.
		if _, err := db.GlobalStats(); err == nil {
			t.Error("GlobalStats should fail without chunks table")
		}
		// CacheStats: path scan succeeds, per-batch chunk SUM fails.
		if _, err := db.CacheStats(c.ID); err == nil {
			t.Error("CacheStats should fail without chunks table")
		}
		// GC: LiveChunkSet (paths) succeeds, AllChunks fails.
		if _, _, err := db.GC(context.Background(), failDelete{}, 1); err == nil {
			t.Error("GC should fail without chunks table")
		}
		// enforceCap: paths scan succeeds, chunkSizes fails.
		if _, err := db.EnforceGlobalCap(1); err == nil {
			t.Error("EnforceGlobalCap should fail without chunks table")
		}
		if _, err := db.deleteChunkRowIf("x", 1); err == nil {
			t.Error("deleteChunkRowIf should fail without chunks table")
		}
		if err := db.TouchChunks([]string{"x"}, 1); err == nil {
			t.Error("TouchChunks should fail without chunks table")
		}
	})

	t.Run("no paths table", func(t *testing.T) {
		db := openTest(t)
		db.CreateCache("c", true, 40)
		exec(t, db, `DROP TABLE paths`)
		if _, err := db.GlobalStats(); err == nil {
			t.Error("GlobalStats should fail without paths table")
		}
		if _, err := db.EvictPathsOlderThan(1); err == nil {
			t.Error("EvictPathsOlderThan should fail without paths table")
		}
	})

	t.Run("no sessions/metrics/tokens tables", func(t *testing.T) {
		db := openTest(t)
		exec(t, db, `DROP TABLE sessions`)
		exec(t, db, `DROP TABLE metrics_minutes`)
		exec(t, db, `DROP TABLE tokens`)
		if _, _, err := db.CreateToken("x", nil, nil, 0); err == nil {
			t.Error("CreateToken should fail without tokens table")
		}
		if err := db.CreateSession("s", 1, time.Now().Add(time.Hour)); err == nil {
			t.Error("CreateSession should fail without sessions table")
		}
		if err := db.AddMetricMinute(MetricMinute{TS: 1}); err == nil {
			t.Error("AddMetricMinute should fail without metrics table")
		}
	})
}

// addColumnIfMissing on a nonexistent table: pragma_table_info returns no rows
// (no error), so the ALTER runs and fails.
func TestAddColumnIfMissingAlterError(t *testing.T) {
	raw, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "x.db")+pragmas)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if err := addColumnIfMissing(raw, false, "nonexistent", "col", "INTEGER"); err == nil {
		t.Fatal("ALTER on a missing table should error")
	}
	// query error path: closed handle
	raw.Close()
	if err := addColumnIfMissing(raw, false, "t", "col", "INTEGER"); err == nil {
		t.Fatal("pragma query on closed db should error")
	}
}

// A view named like a migrated table: CREATE TABLE IF NOT EXISTS is a no-op,
// but the ALTER to add missing columns fails — migrate must surface it.
func TestMigrateAlterError(t *testing.T) {
	raw, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "x.db")+pragmas)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`CREATE VIEW chunks AS SELECT 'h' AS hash, 1 AS size, 'k' AS storage_key`); err != nil {
		t.Fatal(err)
	}
	if err := migrate(raw, false); err == nil {
		t.Fatal("migrate over a chunks view should error")
	}
}

// touchOnDelete simulates the GC-vs-push race: deleting one chunk's blob
// re-stamps the other, so the sweep's transactional re-check must skip it.
type touchOnDelete struct {
	db    *DB
	other map[string]string // hash -> hash to touch when its blob is deleted
}

func (s touchOnDelete) Put(context.Context, string, io.Reader) error       { return nil }
func (s touchOnDelete) Get(context.Context, string) (io.ReadCloser, error) { return nil, ErrNotFound }
func (s touchOnDelete) Has(context.Context, string) (bool, error)          { return false, nil }
func (s touchOnDelete) Delete(_ context.Context, key string) error {
	if o, ok := s.other[key]; ok {
		return s.db.TouchChunks([]string{o}, 1<<62)
	}
	return nil
}

// dropOnDelete kills the chunks table on the first blob delete, so the next
// deleteChunkRowIf in the sweep errors.
type dropOnDelete struct {
	t  *testing.T
	db *DB
}

func (s dropOnDelete) Put(context.Context, string, io.Reader) error       { return nil }
func (s dropOnDelete) Get(context.Context, string) (io.ReadCloser, error) { return nil, ErrNotFound }
func (s dropOnDelete) Has(context.Context, string) (bool, error)          { return false, nil }
func (s dropOnDelete) Delete(context.Context, string) error {
	exec(s.t, s.db, `DROP TABLE IF EXISTS chunks`)
	return nil
}

// setGCBatch forces a sweep batch size so per-chunk race interleavings stay
// reproducible now that GC batches row deletes.
func setGCBatch(t *testing.T, n int) {
	t.Helper()
	old := gcBatchSize
	gcBatchSize = n
	t.Cleanup(func() { gcBatchSize = old })
}

func TestGCRowDeleteErrorMidSweep(t *testing.T) {
	db := openTest(t)
	setGCBatch(t, 1)
	db.PutChunk("a", 10, 5, "ka", 100)
	db.PutChunk("b", 10, 7, "kb", 100)
	// First orphan sweeps fine; its blob delete drops the table, so the second
	// orphan's row delete errors and GC must surface it.
	deleted, _, err := db.GC(context.Background(), dropOnDelete{t: t, db: db}, 5000)
	if err == nil {
		t.Fatal("GC should surface the row-delete error")
	}
	if deleted != 1 {
		t.Fatalf("deleted=%d, want 1 before the failure", deleted)
	}
}

func TestGCSkipsChunkRestampedMidSweep(t *testing.T) {
	db := openTest(t)
	setGCBatch(t, 1)
	db.PutChunk("a", 10, 5, "ka", 100)
	db.PutChunk("b", 10, 7, "kb", 100)
	st := touchOnDelete{db: db, other: map[string]string{"ka": "b", "kb": "a"}}

	// Whichever chunk the sweep deletes first, its blob delete re-stamps the
	// other; the second deleteChunkRowIf then refuses (ok=false → continue).
	deleted, _, err := db.GC(context.Background(), st, 5000)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Fatalf("deleted=%d, want 1 (second chunk spared by re-stamp)", deleted)
	}
	if !db.HasChunk("a") && !db.HasChunk("b") {
		t.Fatal("re-stamped chunk was swept")
	}
}
