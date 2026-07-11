// Package store is the metadata DB: pure-Go SQLite in WAL mode with a single
// writer goroutine. All writes funnel through one goroutine, so SQLITE_BUSY is
// structurally impossible and concurrent pushes never stall — the whole reason
// xilo exists. Reads use a separate WAL connection pool and are never blocked.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type DB struct {
	r  *sql.DB // read pool (WAL readers never block on the writer)
	wr chan wtask
	// pg is set when backed by Postgres: r is a normal concurrent pool and
	// writes run directly on it (MVCC — no single-writer funnel needed).
	pg bool
}

type wtask struct {
	fn   func(*sql.Tx) error
	resp chan error
}

const pragmaBase = "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)" +
	"&_pragma=foreign_keys(1)&_pragma=synchronous("

// pragmas is the default (synchronous=NORMAL) DSN suffix, exactly what Open
// uses — tests build raw pre-migration DBs with it.
const pragmas = pragmaBase + "1)"

// Open opens (creating if needed) the sqlite database at path with
// synchronous=NORMAL: no fsync per commit — a power loss can drop the last
// few acknowledged writes (the DB itself stays consistent). The right
// default for a cache, where a lost push heals on the next run.
func Open(path string) (*DB, error) { return open(path, 1) }

// OpenDurable is Open with synchronous=FULL: every commit fsyncs, so an
// acknowledged push survives power loss, at a per-write latency cost.
func OpenDurable(path string) (*DB, error) { return open(path, 2) }

func open(path string, sync int) (*DB, error) {
	dsn := fmt.Sprintf("file:%s%s%d)", path, pragmaBase, sync)
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

	if err := migrate(w, false); err != nil {
		w.Close()
		r.Close()
		return nil, err
	}

	db := &DB{r: r, wr: make(chan wtask)}
	go db.writer(w)
	return db, nil
}

// OpenPostgres connects to a PostgreSQL database (dsn: postgres://…). Unlike
// SQLite there is no single-writer funnel — Postgres handles concurrent
// writers natively — so reads and writes share one pool.
func OpenPostgres(dsn string) (*DB, error) {
	connector, err := newPGConnector(dsn)
	if err != nil {
		return nil, err
	}
	pool := sql.OpenDB(connector)
	n := max(4, 2*runtime.NumCPU())
	pool.SetMaxOpenConns(n)
	pool.SetMaxIdleConns(n)
	if err := migrate(pool, true); err != nil {
		pool.Close()
		return nil, err
	}
	return &DB{r: pool, pg: true}, nil
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
// mutation in this package goes through here. Safe to call during/after Close:
// fire-and-forget writers (TouchPath, metrics flush, GC tick) can race
// shutdown, and a send on the closed channel must degrade to an error, not a
// process-killing panic.
func (db *DB) write(fn func(*sql.Tx) error) (err error) {
	if db.pg {
		return db.writePG(fn)
	}
	defer func() {
		if recover() != nil {
			err = errors.New("store: closed")
		}
	}()
	resp := make(chan error, 1)
	db.wr <- wtask{fn: fn, resp: resp}
	return <-resp
}

// writePG runs fn in a transaction straight on the pool, with the same
// panic-rolls-back contract as the SQLite writer goroutine.
func (db *DB) writePG(fn func(*sql.Tx) error) (err error) {
	tx, err := db.r.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if rec := recover(); rec != nil {
			tx.Rollback()
			err = fmt.Errorf("write panic: %v", rec)
		}
	}()
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (db *DB) Close() error {
	if !db.pg {
		close(db.wr)
	}
	err := db.r.Close()
	return err
}

// pgDDL translates the canonical (SQLite-syntax) schema below for Postgres.
// SQLite's INTEGER is 64-bit, so plain INTEGER columns become BIGINT; the
// rowid-alias primary keys become identity columns.
func pgDDL(s string) string {
	s = strings.ReplaceAll(s, "INTEGER PRIMARY KEY", "BIGINT GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY")
	s = strings.ReplaceAll(s, "INTEGER", "BIGINT")
	s = strings.ReplaceAll(s, "BLOB", "BYTEA")
	s = strings.ReplaceAll(s, "REAL", "DOUBLE PRECISION")
	return s
}

func migrate(w *sql.DB, pg bool) error {
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
		`CREATE TABLE IF NOT EXISTS users (
			id            INTEGER PRIMARY KEY,
			username      TEXT UNIQUE NOT NULL,
			password_hash TEXT NOT NULL,
			role          TEXT NOT NULL DEFAULT 'member',
			totp_secret   BLOB,
			totp_enabled  INTEGER NOT NULL DEFAULT 0,
			created       INTEGER NOT NULL DEFAULT 0
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
		`CREATE TABLE IF NOT EXISTS passkeys (
			id         INTEGER PRIMARY KEY,
			name       TEXT NOT NULL,
			credential BLOB NOT NULL,
			created    INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS sessions (
			id      TEXT PRIMARY KEY,
			expires INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS metrics_minutes (
			ts     INTEGER PRIMARY KEY,
			req    REAL NOT NULL,
			lat    REAL NOT NULL,
			bps    REAL NOT NULL,
			stored INTEGER NOT NULL
		)`,
	}
	for _, s := range stmts {
		if pg {
			s = pgDDL(s)
		}
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
		{"passkeys", "user_id", "INTEGER NOT NULL DEFAULT 0"},
		{"sessions", "user_id", "INTEGER NOT NULL DEFAULT 0"},
	}
	for _, a := range adds {
		if err := addColumnIfMissing(w, pg, a.table, a.col, a.def); err != nil {
			return fmt.Errorf("migrate %s.%s: %w", a.table, a.col, err)
		}
	}

	if err := migrateAdminToUsers(w, pg); err != nil {
		return fmt.Errorf("migrate admin→users: %w", err)
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

// migrateAdminToUsers converts a pre-1.0 singleton `admin` row into the first
// user (username "admin", role admin), claims the existing passkeys for it,
// and drops the old table. Sessions are wiped — one forced re-login beats
// carrying ownerless cookies forward.
func migrateAdminToUsers(w *sql.DB, pg bool) error {
	exists := `SELECT 1 FROM sqlite_master WHERE type='table' AND name='admin'`
	if pg {
		exists = `SELECT 1 FROM information_schema.tables WHERE table_name='admin' AND table_schema=current_schema()`
	}
	var one int
	if err := w.QueryRow(exists).Scan(&one); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil // fresh database, nothing to migrate
		}
		return err
	}
	var hash string
	var totpSecret []byte
	var totpEnabled int
	err := w.QueryRow(`SELECT password_hash, totp_secret, totp_enabled FROM admin WHERE id=1`).
		Scan(&hash, &totpSecret, &totpEnabled)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil {
		// The pg pool rewrites `?` at the driver boundary, so one placeholder
		// style serves both dialects here too.
		var uid int64
		if err := w.QueryRow(
			`INSERT INTO users (username,password_hash,role,totp_secret,totp_enabled,created)
			 VALUES ('admin',?,'admin',?,?,?) RETURNING id`,
			hash, totpSecret, totpEnabled, time.Now().Unix()).Scan(&uid); err != nil {
			return err
		}
		if _, err := w.Exec(`UPDATE passkeys SET user_id=? WHERE user_id=0`, uid); err != nil {
			return err
		}
		if _, err := w.Exec(`DELETE FROM sessions`); err != nil {
			return err
		}
	}
	_, err = w.Exec(`DROP TABLE admin`)
	return err
}

// addColumnIfMissing runs ALTER TABLE ADD COLUMN only when the column is absent
// (SQLite has no ADD COLUMN IF NOT EXISTS; Postgres does).
func addColumnIfMissing(w *sql.DB, pg bool, table, col, def string) error {
	if pg {
		_, err := w.Exec(`ALTER TABLE ` + table + ` ADD COLUMN IF NOT EXISTS ` + col + ` ` + pgDDL(def))
		return err
	}
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
