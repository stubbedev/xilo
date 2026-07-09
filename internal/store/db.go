// Package store is the metadata DB: pure-Go SQLite in WAL mode with a single
// writer goroutine. All writes funnel through one goroutine, so SQLITE_BUSY is
// structurally impossible and concurrent pushes never stall — the whole reason
// xilo exists. Reads use a separate WAL connection pool and are never blocked.
package store

import (
	"database/sql"
	"fmt"
	"runtime"

	_ "modernc.org/sqlite"
)

type DB struct {
	r  *sql.DB // read pool (WAL readers never block on the writer)
	wr chan wtask
}

type wtask struct {
	fn   func(*sql.Tx) error
	resp chan error
}

const pragmas = "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)" +
	"&_pragma=foreign_keys(1)&_pragma=synchronous(1)"

// Open opens (creating if needed) the sqlite database at path, runs migrations,
// and starts the writer goroutine.
func Open(path string) (*DB, error) {
	dsn := "file:" + path + pragmas
	w, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	w.SetMaxOpenConns(1) // the single writer

	r, err := sql.Open("sqlite", dsn)
	if err != nil {
		w.Close()
		return nil, err
	}
	// Keep hot reader connections warm instead of churning per request.
	n := max(4, runtime.NumCPU())
	r.SetMaxOpenConns(n)
	r.SetMaxIdleConns(n)

	if err := migrate(w); err != nil {
		w.Close()
		r.Close()
		return nil, err
	}

	db := &DB{r: r, wr: make(chan wtask)}
	go db.writer(w)
	return db, nil
}

func (db *DB) writer(w *sql.DB) {
	defer w.Close() // the writer goroutine owns w
	for t := range db.wr {
		db.runWrite(w, t)
	}
}

// runWrite executes one write task, recovering from a panic inside fn so a
// single bad write can't wedge the writer goroutine forever. Crucially the
// panic path ROLLS BACK the transaction — otherwise the single writer
// connection stays checked out and the next Begin deadlocks.
func (db *DB) runWrite(w *sql.DB, t wtask) {
	tx, err := w.Begin()
	if err != nil {
		t.resp <- err
		return
	}
	defer func() {
		if rec := recover(); rec != nil {
			tx.Rollback()
			t.resp <- fmt.Errorf("write panic: %v", rec)
		}
	}()
	if err := t.fn(tx); err != nil {
		tx.Rollback()
		t.resp <- err
		return
	}
	t.resp <- tx.Commit()
}

// write runs fn inside a transaction on the single writer connection. Every
// mutation in this package goes through here.
func (db *DB) write(fn func(*sql.Tx) error) error {
	resp := make(chan error, 1)
	db.wr <- wtask{fn: fn, resp: resp}
	return <-resp
}

func (db *DB) Close() error {
	close(db.wr)
	err := db.r.Close()
	return err
}

func migrate(w *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS caches (
			id        INTEGER PRIMARY KEY,
			name      TEXT UNIQUE NOT NULL,
			public    INTEGER NOT NULL DEFAULT 1,
			priority  INTEGER NOT NULL DEFAULT 40,
			retention INTEGER NOT NULL DEFAULT 0,
			max_bytes INTEGER NOT NULL DEFAULT 0,
			pubkey    TEXT NOT NULL,
			privkey   BLOB NOT NULL,
			created   INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS admin (
			id            INTEGER PRIMARY KEY CHECK (id = 1),
			password_hash TEXT NOT NULL,
			totp_secret   BLOB,
			totp_enabled  INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS chunks (
			hash        TEXT PRIMARY KEY,
			size        INTEGER NOT NULL,
			csize       INTEGER NOT NULL DEFAULT 0,
			storage_key TEXT NOT NULL,
			created     INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS paths (
			id         INTEGER PRIMARY KEY,
			cache_id   INTEGER NOT NULL REFERENCES caches(id) ON DELETE CASCADE,
			store_hash TEXT NOT NULL,
			store_path TEXT NOT NULL,
			nar_hash   TEXT NOT NULL,
			nar_size   INTEGER NOT NULL,
			deriver    TEXT NOT NULL DEFAULT '',
			refs       TEXT NOT NULL DEFAULT '',
			chunks     TEXT NOT NULL DEFAULT '',
			accessed   INTEGER NOT NULL,
			UNIQUE(cache_id, store_hash)
		)`,
		`CREATE TABLE IF NOT EXISTS tokens (
			id      INTEGER PRIMARY KEY,
			name    TEXT NOT NULL,
			hash    TEXT UNIQUE NOT NULL,
			caches  TEXT NOT NULL DEFAULT '*',
			perms   TEXT NOT NULL DEFAULT 'pull',
			revoked INTEGER NOT NULL DEFAULT 0,
			expires INTEGER NOT NULL DEFAULT 0,
			created INTEGER NOT NULL
		)`,
	}
	for _, s := range stmts {
		if _, err := w.Exec(s); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}

	// Additive migrations for databases created by an earlier version. CREATE
	// TABLE IF NOT EXISTS above does not add columns to a pre-existing table, so
	// bring older schemas forward here. Each is a no-op once the column exists.
	adds := []struct{ table, col, def string }{
		{"caches", "retention", "INTEGER NOT NULL DEFAULT 0"},
		{"caches", "max_bytes", "INTEGER NOT NULL DEFAULT 0"},
		{"chunks", "csize", "INTEGER NOT NULL DEFAULT 0"},
		{"chunks", "created", "INTEGER NOT NULL DEFAULT 0"},
		{"tokens", "expires", "INTEGER NOT NULL DEFAULT 0"},
	}
	for _, a := range adds {
		if err := addColumnIfMissing(w, a.table, a.col, a.def); err != nil {
			return fmt.Errorf("migrate %s.%s: %w", a.table, a.col, err)
		}
	}

	// Indexes last — they may reference columns the ALTERs just added.
	for _, s := range []string{
		`CREATE INDEX IF NOT EXISTS idx_paths_accessed ON paths(accessed)`,
		`CREATE INDEX IF NOT EXISTS idx_chunks_created ON chunks(created)`,
	} {
		if _, err := w.Exec(s); err != nil {
			return fmt.Errorf("migrate index: %w", err)
		}
	}
	return nil
}

// addColumnIfMissing runs ALTER TABLE ADD COLUMN only when the column is absent
// (SQLite has no ADD COLUMN IF NOT EXISTS).
func addColumnIfMissing(w *sql.DB, table, col, def string) error {
	rows, err := w.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		if name == col {
			return nil // already present
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = w.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + col + ` ` + def)
	return err
}
