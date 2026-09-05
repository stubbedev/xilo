package store

import (
	"database/sql"
	"errors"
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
