package store

import (
	"crypto/ed25519"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/stubbedev/xilo/internal/narinfo"
)

var ErrNotFound = errors.New("not found")

type Cache struct {
	ID        int64
	Name      string
	Public    bool
	Priority  int
	Retention int64  // per-cache retention seconds; 0 = use global
	PubKey    string // "<name>:<base64 pub>"
	PrivKey   ed25519.PrivateKey
	Created   int64
}

const cacheCols = `id,name,public,priority,retention,pubkey,privkey,created`

type Path struct {
	StorePath string
	NarHash   string // sha256:<base32>
	NarSize   uint64
	Deriver   string   // base name or ""
	Refs      []string // full store paths
	Chunks    []string // chunk hashes, in NAR order
}

// ---- caches ----

// CreateCache generates an ed25519 keypair (key name = cache name) and inserts
// the cache. The signing key never leaves the server.
func (db *DB) CreateCache(name string, public bool, priority int) (*Cache, error) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		return nil, err
	}
	c := &Cache{
		Name:     name,
		Public:   public,
		Priority: priority,
		PubKey:   narinfo.PublicKeyString(name, pub),
		PrivKey:  priv,
		Created:  time.Now().Unix(),
	}
	err = db.write(func(tx *sql.Tx) error {
		res, err := tx.Exec(
			`INSERT INTO caches (name, public, priority, pubkey, privkey, created) VALUES (?,?,?,?,?,?)`,
			c.Name, b2i(c.Public), c.Priority, c.PubKey, []byte(c.PrivKey), c.Created)
		if err != nil {
			return err
		}
		c.ID, err = res.LastInsertId()
		return err
	})
	if err != nil {
		return nil, err
	}
	return c, nil
}

func scanCache(row interface{ Scan(...any) error }) (*Cache, error) {
	var c Cache
	var pub int
	var priv []byte
	if err := row.Scan(&c.ID, &c.Name, &pub, &c.Priority, &c.Retention, &c.PubKey, &priv, &c.Created); err != nil {
		return nil, err
	}
	c.Public = pub != 0
	c.PrivKey = ed25519.PrivateKey(priv)
	return &c, nil
}

func (db *DB) GetCache(name string) (*Cache, error) {
	row := db.r.QueryRow(`SELECT `+cacheCols+` FROM caches WHERE name=?`, name)
	c, err := scanCache(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return c, err
}

func (db *DB) ListCaches() ([]Cache, error) {
	rows, err := db.r.Query(`SELECT ` + cacheCols + ` FROM caches ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Cache
	for rows.Next() {
		c, err := scanCache(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

// UpdateCache changes the mutable cache settings (visibility, priority,
// per-cache retention seconds).
func (db *DB) UpdateCache(id int64, public bool, priority int, retention int64) error {
	return db.write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE caches SET public=?, priority=?, retention=? WHERE id=?`,
			b2i(public), priority, retention, id)
		return err
	})
}

// RotateKey generates a fresh signing keypair for a cache. Invalidates the
// previously-distributed trusted-public-key.
func (db *DB) RotateKey(id int64, name string) (*Cache, error) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		return nil, err
	}
	pubStr := narinfo.PublicKeyString(name, pub)
	err = db.write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE caches SET pubkey=?, privkey=? WHERE id=?`, pubStr, []byte(priv), id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return db.GetCache(name)
}

// DeleteCache removes a cache and its path rows (ON DELETE CASCADE). Orphaned
// chunks are reclaimed by the next GC sweep.
func (db *DB) DeleteCache(id int64) error {
	return db.write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM caches WHERE id=?`, id)
		return err
	})
}

// ---- chunks ----

// PutChunk records a stored chunk (uncompressed + compressed sizes).
// Idempotent (INSERT OR IGNORE). created stamps the ingest time so GC can grant
// a grace window and never sweep a chunk mid-push.
func (db *DB) PutChunk(hash string, size, csize int64, storageKey string, now int64) error {
	return db.write(func(tx *sql.Tx) error {
		_, err := tx.Exec(
			`INSERT OR IGNORE INTO chunks (hash,size,csize,storage_key,created) VALUES (?,?,?,?,?)`,
			hash, size, csize, storageKey, now)
		return err
	})
}

// TouchPath bumps a path's accessed time (LRU by last pull). Best-effort: only
// writes when the recorded time is older than minAge seconds, to avoid a write
// per narinfo request.
func (db *DB) TouchPath(cacheID int64, storeHash string, now, minAge int64) {
	// Cheap read on the pool first; only enqueue a write if stale.
	var accessed int64
	err := db.r.QueryRow(`SELECT accessed FROM paths WHERE cache_id=? AND store_hash=?`,
		cacheID, storeHash).Scan(&accessed)
	if err != nil || now-accessed < minAge {
		return
	}
	_ = db.write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE paths SET accessed=? WHERE cache_id=? AND store_hash=?`,
			now, cacheID, storeHash)
		return err
	})
}

// HasChunk reports whether a chunk row exists.
func (db *DB) HasChunk(hash string) bool {
	var one int
	err := db.r.QueryRow(`SELECT 1 FROM chunks WHERE hash=?`, hash).Scan(&one)
	return err == nil
}

// MissingChunks returns the subset of hashes not yet present.
func (db *DB) MissingChunks(hashes []string) ([]string, error) {
	present, err := db.presentSet("chunks", "hash", hashes)
	if err != nil {
		return nil, err
	}
	return diff(hashes, present), nil
}

// ---- paths ----

// MissingPaths returns the subset of storeHashes not present in the cache.
func (db *DB) MissingPaths(cacheID int64, storeHashes []string) ([]string, error) {
	present := map[string]bool{}
	err := db.eachBatch(storeHashes, func(batch []string) error {
		q := `SELECT store_hash FROM paths WHERE cache_id=? AND store_hash IN (` + placeholders(len(batch)) + `)`
		args := append([]any{cacheID}, toArgs(batch)...)
		rows, err := db.r.Query(q, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var h string
			if err := rows.Scan(&h); err != nil {
				return err
			}
			present[h] = true
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return diff(storeHashes, present), nil
}

// PutPath registers a store path in a cache. store_hash is the 32-char hash part
// of the store path base name.
func (db *DB) PutPath(cacheID int64, storeHash string, p *Path) error {
	return db.write(func(tx *sql.Tx) error {
		_, err := tx.Exec(
			`INSERT INTO paths (cache_id,store_hash,store_path,nar_hash,nar_size,deriver,refs,chunks,accessed)
			 VALUES (?,?,?,?,?,?,?,?,?)
			 ON CONFLICT(cache_id,store_hash) DO UPDATE SET
			   store_path=excluded.store_path, nar_hash=excluded.nar_hash,
			   nar_size=excluded.nar_size, deriver=excluded.deriver,
			   refs=excluded.refs, chunks=excluded.chunks, accessed=excluded.accessed`,
			cacheID, storeHash, p.StorePath, p.NarHash, int64(p.NarSize), p.Deriver,
			strings.Join(p.Refs, "\n"), strings.Join(p.Chunks, "\n"), time.Now().Unix())
		return err
	})
}

func (db *DB) GetPath(cacheID int64, storeHash string) (*Path, error) {
	row := db.r.QueryRow(
		`SELECT store_path,nar_hash,nar_size,deriver,refs,chunks FROM paths WHERE cache_id=? AND store_hash=?`,
		cacheID, storeHash)
	var p Path
	var narSize int64
	var refs, chunks string
	err := row.Scan(&p.StorePath, &p.NarHash, &narSize, &p.Deriver, &refs, &chunks)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	p.NarSize = uint64(narSize)
	p.Refs = splitLines(refs)
	p.Chunks = splitLines(chunks)
	return &p, nil
}

// ChunkKeys returns the storage keys for chunk hashes, preserving order. Batched
// (one query per ~batchVars hashes) instead of N+1 point lookups.
func (db *DB) ChunkKeys(hashes []string) ([]string, error) {
	byHash := make(map[string]string, len(hashes))
	err := db.eachBatch(hashes, func(batch []string) error {
		q := `SELECT hash, storage_key FROM chunks WHERE hash IN (` + placeholders(len(batch)) + `)`
		rows, err := db.r.Query(q, toArgs(batch)...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var h, k string
			if err := rows.Scan(&h, &k); err != nil {
				return err
			}
			byHash[h] = k
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	keys := make([]string, len(hashes))
	for i, h := range hashes {
		k, ok := byHash[h]
		if !ok {
			return nil, ErrNotFound
		}
		keys[i] = k
	}
	return keys, nil
}

// ---- helpers ----

func (db *DB) presentSet(table, col string, vals []string) (map[string]bool, error) {
	present := map[string]bool{}
	err := db.eachBatch(vals, func(batch []string) error {
		q := `SELECT ` + col + ` FROM ` + table + ` WHERE ` + col + ` IN (` + placeholders(len(batch)) + `)`
		rows, err := db.r.Query(q, toArgs(batch)...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				return err
			}
			present[v] = true
		}
		return rows.Err()
	})
	return present, err
}

// batchVars bounds placeholders per query, well under SQLite's
// SQLITE_MAX_VARIABLE_NUMBER (32766 in modernc), leaving room for extra args.
const batchVars = 900

// eachBatch calls fn on successive slices of items, each at most batchVars long.
func (db *DB) eachBatch(items []string, fn func([]string) error) error {
	for i := 0; i < len(items); i += batchVars {
		end := min(i+batchVars, len(items))
		if err := fn(items[i:end]); err != nil {
			return err
		}
	}
	return nil
}

func toArgs(vals []string) []any {
	args := make([]any, len(vals))
	for i, v := range vals {
		args[i] = v
	}
	return args
}

func diff(all []string, present map[string]bool) []string {
	var out []string
	seen := map[string]bool{}
	for _, v := range all {
		if !present[v] && !seen[v] {
			out = append(out, v)
			seen[v] = true
		}
	}
	return out
}

func placeholders(n int) string { return strings.TrimSuffix(strings.Repeat("?,", n), ",") }

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
