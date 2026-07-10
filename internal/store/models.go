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
	MaxBytes  int64  // per-cache storage cap (compressed); 0 = unlimited
	PubKey    string // "<name>:<base64 pub>"
	PrivKey   ed25519.PrivateKey
	Created   int64
}

const cacheCols = `id,name,public,priority,retention,max_bytes,pubkey,privkey,created`

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
	if err := row.Scan(&c.ID, &c.Name, &pub, &c.Priority, &c.Retention, &c.MaxBytes, &c.PubKey, &priv, &c.Created); err != nil {
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
// per-cache retention seconds, storage cap bytes).
func (db *DB) UpdateCache(id int64, public bool, priority int, retention, maxBytes int64) error {
	return db.write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE caches SET public=?, priority=?, retention=?, max_bytes=? WHERE id=?`,
			b2i(public), priority, retention, maxBytes, id)
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
// Idempotent; a re-upload re-stamps created so the GC grace window restarts.
func (db *DB) PutChunk(hash string, size, csize int64, storageKey string, now int64) error {
	return db.write(func(tx *sql.Tx) error {
		_, err := tx.Exec(
			`INSERT INTO chunks (hash,size,csize,storage_key,created) VALUES (?,?,?,?,?)
			 ON CONFLICT(hash) DO UPDATE SET created=excluded.created`,
			hash, size, csize, storageKey, now)
		return err
	})
}

// TouchChunks re-stamps created on existing chunks. Called whenever the server
// promises a pusher that these chunks are present (so it will skip uploading
// them) — the restarted grace window guarantees GC can't sweep them before the
// push registers its path.
func (db *DB) TouchChunks(hashes []string, now int64) error {
	if len(hashes) == 0 {
		return nil
	}
	return db.eachBatch(hashes, func(batch []string) error {
		return db.write(func(tx *sql.Tx) error {
			args := append([]any{now}, toArgs(batch)...)
			_, err := tx.Exec(`UPDATE chunks SET created=? WHERE hash IN (`+placeholders(len(batch))+`)`, args...)
			return err
		})
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

// PathInfo is a dashboard listing row: the store path, its NAR size, and when
// it was last pulled.
type PathInfo struct {
	StorePath string
	NarSize   int64
	Accessed  int64
}

// fuzzyPattern turns a search term into a LIKE pattern that matches its
// characters in order with anything between ("ffx" → %f%f%x%). LIKE wildcards
// in the term are escaped.
func fuzzyPattern(term string) string {
	var b strings.Builder
	b.WriteByte('%')
	for _, r := range term {
		if r == '%' || r == '_' || r == '\\' {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
		b.WriteByte('%')
	}
	return b.String()
}

// SearchPaths lists a page of a cache's paths. The query is split on
// whitespace; every term must fuzzy-match the store path (characters in
// order, case-insensitive). sortKey (path|size|pulled) + sortDir (asc|desc)
// pick an explicit order; with no sortKey, substring hits rank above pure
// fuzzy ones and ties order by most recently pulled. total is the match
// count before limit/offset.
func (db *DB) SearchPaths(cacheID int64, q string, limit, offset int, sortKey, sortDir string) (paths []PathInfo, total int64, err error) {
	where := `cache_id=?`
	args := []any{cacheID}
	rank := `0`
	var rankArgs []any
	for _, term := range strings.Fields(q) {
		where += ` AND store_path LIKE ? ESCAPE '\'`
		args = append(args, fuzzyPattern(term))
		rank += ` + (instr(lower(store_path), lower(?)) > 0)`
		rankArgs = append(rankArgs, term)
	}
	// Explicit column sort wins; otherwise fuzzy rank (when searching) then
	// recency. Column and direction come from a whitelist — never the query.
	dir := ` DESC`
	if sortDir == "asc" {
		dir = ` ASC`
	}
	var order string
	switch sortKey {
	case "path":
		// Order by the name after "/nix/store/<32-char-hash>-" (char 45) so
		// the column sorts by package name, not by hash noise.
		order = `substr(store_path, 45) COLLATE NOCASE` + dir
		rankArgs = nil
	case "size":
		order = `nar_size` + dir + `, accessed DESC`
		rankArgs = nil
	case "pulled":
		order = `accessed` + dir
		rankArgs = nil
	default:
		// A bare integer in ORDER BY is a column ordinal to SQLite, so the
		// rank expression is only included when there are search terms.
		order = `accessed DESC`
		if len(rankArgs) > 0 {
			order = `(` + rank + `) DESC, accessed DESC`
		}
	}
	args = append(args, rankArgs...)
	args = append(args, limit, offset)
	rows, err := db.r.Query(
		`SELECT store_path, nar_size, accessed, COUNT(*) OVER ()
		   FROM paths
		  WHERE `+where+`
		  ORDER BY `+order+` LIMIT ? OFFSET ?`,
		args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var p PathInfo
		if err := rows.Scan(&p.StorePath, &p.NarSize, &p.Accessed, &total); err != nil {
			return nil, 0, err
		}
		paths = append(paths, p)
	}
	return paths, total, rows.Err()
}

// ChunkKeys returns the storage keys for chunk hashes, preserving order. Batched
// (one query per ~batchVars hashes) instead of N+1 point lookups.
func (db *DB) ChunkKeys(hashes []string) ([]ChunkRef, error) {
	byHash := make(map[string]ChunkRef, len(hashes))
	err := db.eachBatch(hashes, func(batch []string) error {
		q := `SELECT hash, storage_key, size, csize FROM chunks WHERE hash IN (` + placeholders(len(batch)) + `)`
		rows, err := db.r.Query(q, toArgs(batch)...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c ChunkRef
			if err := rows.Scan(&c.Hash, &c.Key, &c.Size, &c.CSize); err != nil {
				return err
			}
			byHash[c.Hash] = c
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	out := make([]ChunkRef, len(hashes))
	for i, h := range hashes {
		c, ok := byHash[h]
		if !ok {
			return nil, ErrNotFound
		}
		out[i] = c
	}
	return out, nil
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
