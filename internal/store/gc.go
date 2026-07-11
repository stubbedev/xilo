package store

import (
	"context"
	"database/sql"
	"fmt"
	"sort"

	"github.com/stubbedev/xilo/internal/storage"
)

// GC deletes stored chunks no path references. Mark-sweep over the
// content-addressed chunk store, with a grace window: only chunks created
// before graceCutoff are eligible, and `created` is re-stamped whenever the
// server promises a chunk's presence to a pusher (missing-chunks / dedup hit /
// put-path), so a chunk an in-flight push relies on is never swept.
//
// Delete order is row THEN blob: the row delete re-checks `created` in the
// same transaction, so a push that just re-stamped the chunk wins the race.
// Once the row is gone the chunk is invisible to dedup — a concurrent push
// re-uploads it — and the worst crash outcome is an orphaned blob (leaked,
// never a registered path pointing at missing data).
func (db *DB) GC(ctx context.Context, st storage.Storage, graceCutoff int64) (deleted int, freed int64, err error) {
	live, err := db.LiveChunkSet()
	if err != nil {
		return 0, 0, err
	}
	all, err := db.AllChunks()
	if err != nil {
		return 0, 0, err
	}
	byHash := map[string]ChunkRef{}
	var cand []string
	for _, c := range all {
		if live[c.Hash] || c.Created >= graceCutoff {
			continue
		}
		byHash[c.Hash] = c
		cand = append(cand, c.Hash)
	}
	for i := 0; i < len(cand); i += gcBatchSize {
		batch := cand[i:min(i+gcBatchSize, len(cand))]
		// One tx per batch: rows not returned were re-stamped by a
		// concurrent push since the snapshot and stay.
		gone, err := db.deleteChunkRowsIf(batch, graceCutoff)
		if err != nil {
			return deleted, freed, err
		}
		if len(gone) == 0 {
			continue
		}
		keys := make([]string, len(gone))
		for j, h := range gone {
			keys[j] = byHash[h].Key
		}
		if err := deleteBlobs(ctx, st, keys); err != nil {
			return deleted, freed, err
		}
		for _, h := range gone {
			freed += byHash[h].CSize
			deleted++
		}
	}
	return deleted, freed, nil
}

// gcBatchSize is how many orphan candidates one sweep batch (one write tx +
// one bulk blob delete) covers. A var so race tests can force tiny batches.
var gcBatchSize = 500

// deleteBlobs takes the backend's bulk path when it has one, else per-key.
func deleteBlobs(ctx context.Context, st storage.Storage, keys []string) error {
	if bd, ok := st.(storage.BulkDeleter); ok {
		return bd.DeleteMany(ctx, keys)
	}
	for _, k := range keys {
		if err := st.Delete(ctx, k); err != nil {
			return err
		}
	}
	return nil
}

// ChunkRef is a stored chunk's identity for GC.
type ChunkRef struct {
	Hash    string
	Key     string
	Size    int64
	CSize   int64
	Created int64
}

// LiveChunkSet returns every chunk hash referenced by at least one path.
func (db *DB) LiveChunkSet() (map[string]bool, error) {
	rows, err := db.r.Query(`SELECT chunks FROM paths`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	live := map[string]bool{}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		for _, h := range splitLines(c) {
			live[h] = true
		}
	}
	return live, rows.Err()
}

// AllChunks lists every stored chunk.
func (db *DB) AllChunks() ([]ChunkRef, error) {
	rows, err := db.r.Query(`SELECT hash, storage_key, size, csize, created FROM chunks`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChunkRef
	for rows.Next() {
		var c ChunkRef
		if err := rows.Scan(&c.Hash, &c.Key, &c.Size, &c.CSize, &c.Created); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// deleteChunkRowsIf removes the given chunk rows still older than the grace
// cutoff — one transaction, per-row re-check via the WHERE clause — and
// returns the hashes actually deleted (DELETE ... RETURNING). The
// transactional re-check is what closes the GC-vs-push race: a push that
// re-stamped a chunk after the sweep snapshot keeps it.
func (db *DB) deleteChunkRowsIf(hashes []string, graceCutoff int64) ([]string, error) {
	var gone []string
	err := db.write(func(tx *sql.Tx) error {
		rows, err := tx.Query(
			`DELETE FROM chunks WHERE hash IN (`+placeholders(len(hashes))+`) AND created<? RETURNING hash`,
			append(toArgs(hashes), graceCutoff)...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var h string
			if err := rows.Scan(&h); err != nil {
				return err
			}
			gone = append(gone, h)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return gone, nil
}

// deleteChunkRowIf is the single-row form, reporting whether it deleted.
func (db *DB) deleteChunkRowIf(hash string, graceCutoff int64) (bool, error) {
	gone, err := db.deleteChunkRowsIf([]string{hash}, graceCutoff)
	return len(gone) > 0, err
}

// EvictPathsOlderThan deletes path rows across all caches not accessed since
// cutoff (unix secs). Chunks orphaned by this are reclaimed on the next GC.
func (db *DB) EvictPathsOlderThan(cutoff int64) (int64, error) {
	return db.evict(`DELETE FROM paths WHERE accessed < ?`, cutoff)
}

// EvictCachePathsOlderThan is the per-cache variant, honoring a cache's own
// retention window.
func (db *DB) EvictCachePathsOlderThan(cacheID, cutoff int64) (int64, error) {
	return db.evict(`DELETE FROM paths WHERE cache_id=? AND accessed < ?`, cacheID, cutoff)
}

type pathRow struct {
	id       int64
	accessed int64
	chunks   []string
}

// EnforceCacheCap evicts least-recently-pulled paths from one cache until the
// compressed size of the distinct chunks it references is <= cap (0 = no cap).
func (db *DB) EnforceCacheCap(cacheID, cap int64) (int, error) {
	if cap <= 0 {
		return 0, nil
	}
	return db.enforceCap(cap, `SELECT id, accessed, chunks FROM paths WHERE cache_id=?`, cacheID)
}

// EnforceGlobalCap evicts least-recently-pulled paths across ALL caches until
// total stored (compressed) size is <= cap (0 = no cap). Reclaimed on the next
// chunk sweep, which runGC runs right after.
func (db *DB) EnforceGlobalCap(cap int64) (int, error) {
	if cap <= 0 {
		return 0, nil
	}
	return db.enforceCap(cap, `SELECT id, accessed, chunks FROM paths`)
}

func (db *DB) enforceCap(cap int64, query string, args ...any) (int, error) {
	rows, err := db.r.Query(query, args...)
	if err != nil {
		return 0, err
	}
	var paths []pathRow
	for rows.Next() {
		var p pathRow
		var chunks string
		if err := rows.Scan(&p.id, &p.accessed, &chunks); err != nil {
			rows.Close()
			return 0, err
		}
		p.chunks = distinct(splitLines(chunks))
		paths = append(paths, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	csize, err := db.chunkSizes()
	if err != nil {
		return 0, err
	}

	evict := evictToFit(paths, csize, cap)
	if len(evict) == 0 {
		return 0, nil
	}
	ids := make([]string, len(evict))
	for i, id := range evict {
		ids[i] = fmt.Sprint(id)
	}
	err = db.eachBatch(ids, func(batch []string) error {
		return db.write(func(tx *sql.Tx) error {
			_, err := tx.Exec(`DELETE FROM paths WHERE id IN (`+placeholders(len(batch))+`)`, toArgs(batch)...)
			return err
		})
	})
	return len(evict), err
}

// evictToFit returns the ids of the oldest-accessed paths to remove so the
// distinct referenced chunks' compressed size drops to <= cap. Refcounts ensure
// a chunk's size only leaves the total when its last referencing path goes.
func evictToFit(paths []pathRow, csize map[string]int64, cap int64) []int64 {
	ref := map[string]int{}
	var total int64
	for _, p := range paths {
		for _, h := range p.chunks {
			if ref[h] == 0 {
				total += csize[h]
			}
			ref[h]++
		}
	}
	if total <= cap {
		return nil
	}
	sort.Slice(paths, func(i, j int) bool { return paths[i].accessed < paths[j].accessed })
	var evict []int64
	for _, p := range paths {
		if total <= cap {
			break
		}
		evict = append(evict, p.id)
		for _, h := range p.chunks {
			ref[h]--
			if ref[h] == 0 {
				total -= csize[h]
			}
		}
	}
	return evict
}

func (db *DB) chunkSizes() (map[string]int64, error) {
	rows, err := db.r.Query(`SELECT hash, csize FROM chunks`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[string]int64{}
	for rows.Next() {
		var h string
		var c int64
		if err := rows.Scan(&h, &c); err != nil {
			return nil, err
		}
		m[h] = c
	}
	return m, rows.Err()
}

func distinct(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func (db *DB) evict(q string, args ...any) (int64, error) {
	var n int64
	err := db.write(func(tx *sql.Tx) error {
		res, err := tx.Exec(q, args...)
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	return n, err
}
