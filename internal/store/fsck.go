package store

import (
	"database/sql"
)

// BrokenPath identifies a path row for fsck reporting/repair.
type BrokenPath struct {
	ID        int64
	StorePath string
}

// PathsWithMissingChunks returns every path whose chunk list references a
// hash that is in extraBad or has no chunks row in the path's cache's storage
// backend — the state fsck exists to find (a registered path that can never
// serve). extraBad entries are "storage/hash" keys.
func (db *DB) PathsWithMissingChunks(extraBad []string) ([]BrokenPath, error) {
	present := map[string]bool{}
	rows, err := db.r.Query(`SELECT storage, hash FROM chunks`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var st, h string
		if err := rows.Scan(&st, &h); err != nil {
			rows.Close()
			return nil, err
		}
		present[st+"/"+h] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, k := range extraBad {
		delete(present, k)
	}

	prows, err := db.r.Query(`SELECT p.id, p.store_path, p.chunks, c.storage FROM paths p JOIN caches c ON c.id = p.cache_id`)
	if err != nil {
		return nil, err
	}
	defer prows.Close()
	var out []BrokenPath
	for prows.Next() {
		var p BrokenPath
		var chunks, st string
		if err := prows.Scan(&p.ID, &p.StorePath, &chunks, &st); err != nil {
			return nil, err
		}
		for _, h := range splitLines(chunks) {
			if !present[st+"/"+h] {
				out = append(out, p)
				break
			}
		}
	}
	return out, prows.Err()
}

// DeletePaths removes path rows by id (fsck repair).
func (db *DB) DeletePaths(ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	return db.eachIDBatch(ids, func(args []any) error {
		return db.write(func(tx *sql.Tx) error {
			_, err := tx.Exec(`DELETE FROM paths WHERE id IN (`+placeholders(len(args))+`)`, args...)
			return err
		})
	})
}

// DeleteChunkRows unconditionally removes chunk rows by hash within a storage
// backend. fsck repair only: unlike the GC sweep there is no grace re-check,
// because the blob is already known missing/corrupt — keeping the row would
// just keep dedup trusting it. Don't run repair concurrently with pushes.
func (db *DB) DeleteChunkRows(storageName string, hashes []string) error {
	if len(hashes) == 0 {
		return nil
	}
	return db.eachBatch(hashes, func(batch []string) error {
		return db.write(func(tx *sql.Tx) error {
			args := append([]any{storageName}, toArgs(batch)...)
			_, err := tx.Exec(`DELETE FROM chunks WHERE storage=? AND hash IN (`+placeholders(len(batch))+`)`, args...)
			return err
		})
	})
}
