package store

import (
	"database/sql"
	"errors"
	"time"
)

// Stats is a cache's dashboard summary, scoped to that cache. LogicalBytes is
// the summed NarSize of its paths (naive per-path storage cost); PhysicalBytes
// is the on-disk (compressed) size of the DISTINCT chunks its paths reference —
// the real footprint after dedup + compression.
type Stats struct {
	Paths         int64
	Chunks        int64 // distinct chunks referenced by this cache
	LogicalBytes  int64 // sum of NarSize
	PhysicalBytes int64 // compressed size of distinct chunks
}

// Global is the server-wide overview for the dashboard. StoredBytes is the true
// on-disk footprint (sum of every distinct chunk's compressed size).
type Global struct {
	Caches       int64
	Paths        int64
	Chunks       int64
	StoredBytes  int64 // compressed, actual disk
	LogicalBytes int64 // sum of NarSize across all paths
}

func (db *DB) GlobalStats() (Global, error) {
	var g Global
	if err := db.r.QueryRow(`SELECT COUNT(*) FROM caches`).Scan(&g.Caches); err != nil {
		return g, err
	}
	if err := db.r.QueryRow(`SELECT COUNT(*), COALESCE(SUM(nar_size),0) FROM paths`).Scan(&g.Paths, &g.LogicalBytes); err != nil {
		return g, err
	}
	if err := db.r.QueryRow(`SELECT COUNT(*), COALESCE(SUM(csize),0) FROM chunks`).Scan(&g.Chunks, &g.StoredBytes); err != nil {
		return g, err
	}
	return g, nil
}

func (db *DB) CacheStats(cacheID int64) (Stats, error) {
	var st Stats
	// The cache's backend, needed to key the chunk lookup below. A missing
	// cache leaves it empty, which simply matches no chunks.
	var storage string
	if err := db.r.QueryRow(`SELECT storage FROM caches WHERE id=?`, cacheID).Scan(&storage); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return st, err
	}
	rows, err := db.r.Query(`SELECT nar_size, chunks FROM paths WHERE cache_id=?`, cacheID)
	if err != nil {
		return st, err
	}
	defer rows.Close()
	distinct := map[string]struct{}{}
	for rows.Next() {
		var narSize int64
		var chunks string
		if err := rows.Scan(&narSize, &chunks); err != nil {
			return st, err
		}
		st.Paths++
		st.LogicalBytes += narSize
		for h := range seqLines(chunks) {
			distinct[h] = struct{}{}
		}
	}
	if err := rows.Err(); err != nil {
		return st, err
	}

	hashes := make([]string, 0, len(distinct))
	for h := range distinct {
		hashes = append(hashes, h)
	}
	// Scoped by storage: chunks are keyed (storage, hash), so leaving the
	// backend out meant this query could not use the primary key and
	// full-scanned the whole chunk table once per batch — 3 s on a cache with
	// 40k chunks, on every dashboard load. It was also wrong wherever two
	// backends hold the same hash, double-counting the footprint.
	//
	const q = `SELECT COALESCE(SUM(csize),0), COUNT(*) FROM chunks WHERE storage=? AND hash IN (`
	err = db.eachBatch(hashes, func(batch []string) error {
		var csize, n int64
		args := append([]any{storage}, toArgs(batch)...)
		if err := db.r.QueryRow(q+placeholders(len(batch))+`)`, args...).Scan(&csize, &n); err != nil {
			return err
		}
		st.PhysicalBytes += csize
		st.Chunks += n
		return nil
	})
	return st, err
}

// statsTTL bounds how stale a stored cache summary may be before the next read
// starts a recompute behind it.
const statsTTL = 30 * time.Second

// StatsFor returns a cache's summary for display, in constant time.
//
// CacheStats itself has to read every path's chunk list and price each hash,
// so its cost grows with the cache and no index can flatten it. This reads the
// stored row instead and, once that row has aged past statsTTL, hands it back
// immediately while a single recompute runs behind it. Only the first ever
// read for a cache pays the full scan.
//
// The numbers are display-only — the per-cache size cap is enforced during GC
// against live rows, never from here — so bounded staleness costs nothing more
// than a briefly late figure on a dashboard. Callers that must be exact (the
// CLI's `cache info`, the JSON API) keep calling CacheStats.
func (db *DB) StatsFor(cacheID int64) (Stats, error) {
	st, computed, ok, err := db.storedStats(cacheID)
	if err != nil {
		return Stats{}, err
	}
	if !ok {
		return db.RefreshStats(cacheID)
	}
	if time.Now().Unix()-computed >= int64(statsTTL.Seconds()) {
		db.refreshBehind(cacheID)
	}
	return st, nil
}

// storedStats reads the stored summary; ok is false when none has been
// computed yet.
func (db *DB) storedStats(cacheID int64) (st Stats, computed int64, ok bool, err error) {
	err = db.r.QueryRow(
		`SELECT paths, logical_bytes, chunks, physical_bytes, computed FROM cache_stats WHERE cache_id=?`,
		cacheID).Scan(&st.Paths, &st.LogicalBytes, &st.Chunks, &st.PhysicalBytes, &computed)
	if errors.Is(err, sql.ErrNoRows) {
		return Stats{}, 0, false, nil
	}
	if err != nil {
		return Stats{}, 0, false, err
	}
	return st, computed, true, nil
}

// RefreshStats recomputes a cache's summary from live rows and stores it.
func (db *DB) RefreshStats(cacheID int64) (Stats, error) {
	st, err := db.CacheStats(cacheID)
	if err != nil {
		return Stats{}, err
	}
	// The freshly computed numbers are returned either way. Storing them is an
	// optimisation for the next reader, and the one way it fails in practice —
	// the cache being deleted while its summary was computed, leaving the
	// insert without a parent row — is not something to report to a caller
	// that already has its answer.
	db.write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO cache_stats (cache_id, paths, logical_bytes, chunks, physical_bytes, computed)
			VALUES (?,?,?,?,?,?)
			ON CONFLICT (cache_id) DO UPDATE SET paths=excluded.paths, logical_bytes=excluded.logical_bytes,
				chunks=excluded.chunks, physical_bytes=excluded.physical_bytes, computed=excluded.computed`,
			cacheID, st.Paths, st.LogicalBytes, st.Chunks, st.PhysicalBytes, time.Now().Unix())
		return err
	})
	return st, nil
}

// refreshBehind recomputes one cache's summary in the background, at most once
// at a time per cache so a burst of page loads cannot stampede the writer.
func (db *DB) refreshBehind(cacheID int64) {
	db.statsMu.Lock()
	if db.refreshing == nil {
		db.refreshing = map[int64]bool{}
	}
	if db.refreshing[cacheID] {
		db.statsMu.Unlock()
		return
	}
	db.refreshing[cacheID] = true
	db.statsMu.Unlock()
	go func() {
		defer func() {
			db.statsMu.Lock()
			delete(db.refreshing, cacheID)
			db.statsMu.Unlock()
		}()
		db.RefreshStats(cacheID)
	}()
}

// GlobalStatsFor is GlobalStats with the same stale-while-revalidate contract
// as StatsFor: the instance totals sum whole tables, which is the one part of
// the dashboard that still grows with the data.
//
// Held in memory rather than a table — it is a single row that costs one
// aggregate to rebuild, so persisting it would buy nothing but a schema.
func (db *DB) GlobalStatsFor() (Global, error) {
	db.statsMu.Lock()
	g, at := db.global, db.globalAt
	db.statsMu.Unlock()
	if at.IsZero() {
		return db.refreshGlobal()
	}
	if time.Since(at) >= statsTTL && db.globalRefreshing.CompareAndSwap(false, true) {
		go func() {
			defer db.globalRefreshing.Store(false)
			db.refreshGlobal()
		}()
	}
	return g, nil
}

func (db *DB) refreshGlobal() (Global, error) {
	g, err := db.GlobalStats()
	if err != nil {
		return Global{}, err
	}
	db.statsMu.Lock()
	db.global, db.globalAt = g, time.Now()
	db.statsMu.Unlock()
	return g, nil
}
