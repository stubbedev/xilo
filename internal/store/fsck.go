package store

import (
	"database/sql"
	"fmt"
)

// BrokenPath identifies a path row for fsck reporting/repair.
type BrokenPath struct {
	ID        int64
	StorePath string
}

// PathsWithMissingChunks returns every path whose chunk list references a
// hash that is in extraBad or has no chunks row — the state fsck exists to
// find (a registered path that can never serve).
func (db *DB) PathsWithMissingChunks(extraBad []string) ([]BrokenPath, error) {
	present := map[string]bool{}
	rows, err := db.r.Query(`SELECT hash FROM chunks`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			rows.Close()
			return nil, err
		}
		present[h] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, h := range extraBad {
		delete(present, h)
	}

	prows, err := db.r.Query(`SELECT id, store_path, chunks FROM paths`)
	if err != nil {
		return nil, err
	}
	defer prows.Close()
	var out []BrokenPath
	for prows.Next() {
		var p BrokenPath
		var chunks string
		if err := prows.Scan(&p.ID, &p.StorePath, &chunks); err != nil {
			return nil, err
		}
		for _, h := range splitLines(chunks) {
			if !present[h] {
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
	strs := make([]string, len(ids))
	for i, id := range ids {
		strs[i] = fmt.Sprint(id)
	}
	return db.eachBatch(strs, func(batch []string) error {
		return db.write(func(tx *sql.Tx) error {
			_, err := tx.Exec(`DELETE FROM paths WHERE id IN (`+placeholders(len(batch))+`)`, toArgs(batch)...)
			return err
		})
	})
}

// DeleteChunkRows unconditionally removes chunk rows by hash. fsck repair
// only: unlike the GC sweep there is no grace re-check, because the blob is
// already known missing/corrupt — keeping the row would just keep dedup
// trusting it. Don't run repair concurrently with pushes.
func (db *DB) DeleteChunkRows(hashes []string) error {
	if len(hashes) == 0 {
		return nil
	}
	return db.eachBatch(hashes, func(batch []string) error {
		return db.write(func(tx *sql.Tx) error {
			_, err := tx.Exec(`DELETE FROM chunks WHERE hash IN (`+placeholders(len(batch))+`)`, toArgs(batch)...)
			return err
		})
	})
}
