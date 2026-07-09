package store

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
		for _, h := range splitLines(chunks) {
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
	err = db.eachBatch(hashes, func(batch []string) error {
		q := `SELECT COALESCE(SUM(csize),0), COUNT(*) FROM chunks WHERE hash IN (` + placeholders(len(batch)) + `)`
		var csize, n int64
		if err := db.r.QueryRow(q, toArgs(batch)...).Scan(&csize, &n); err != nil {
			return err
		}
		st.PhysicalBytes += csize
		st.Chunks += n
		return nil
	})
	return st, err
}
