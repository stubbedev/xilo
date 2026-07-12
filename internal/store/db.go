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
	// key encrypts sensitive columns and keys token hashes; nil = plaintext.
	// Derived from the configured database.salt via SetSalt.
	key []byte
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
	// Table renames must run before CREATE TABLE IF NOT EXISTS below would
	// mint fresh empty tables under the new names and block the rename.
	if err := migrateNamespacesToAccounts(w, pg); err != nil {
		return fmt.Errorf("migrate namespaces→accounts: %w", err)
	}
	// Column renames re-run unguarded: a partially-migrated database may have
	// the tables renamed but old column names (or both columns) left behind.
	for _, tbl := range []struct{ table, from, to string }{
		{"accounts", "name", "slug"},
		{"account_members", "namespace_id", "account_id"},
		{"caches", "namespace_id", "account_id"},
		{"tokens", "namespace_id", "account_id"},
	} {
		if tableExists(w, pg, tbl.table) {
			if err := renameColumnIfPresent(w, pg, tbl.table, tbl.from, tbl.to); err != nil {
				return fmt.Errorf("migrate %s.%s: %w", tbl.table, tbl.from, err)
			}
		}
	}
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS accounts (
			id      INTEGER PRIMARY KEY,
			slug    TEXT UNIQUE NOT NULL,
			kind    TEXT NOT NULL DEFAULT 'org',
			plan_id INTEGER NOT NULL DEFAULT 0,
			created INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS account_members (
			account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
			user_id    INTEGER NOT NULL,
			role       TEXT NOT NULL DEFAULT 'user',
			UNIQUE(account_id, user_id)
		)`,
		`CREATE TABLE IF NOT EXISTS caches (
			id         INTEGER PRIMARY KEY,
			account_id INTEGER NOT NULL DEFAULT 0,
			name       TEXT NOT NULL,
			public     INTEGER NOT NULL DEFAULT 1,
			priority   INTEGER NOT NULL DEFAULT 40,
			retention  INTEGER NOT NULL DEFAULT 0,
			max_bytes  INTEGER NOT NULL DEFAULT 0,
			pubkey     TEXT NOT NULL,
			privkey    BLOB NOT NULL,
			created    INTEGER NOT NULL,
			UNIQUE(account_id, name)
		)`,
		`CREATE TABLE IF NOT EXISTS users (
			id            INTEGER PRIMARY KEY,
			username      TEXT UNIQUE NOT NULL,
			password_hash TEXT NOT NULL,
			role          TEXT NOT NULL DEFAULT 'user',
			totp_secret   BLOB,
			totp_enabled  INTEGER NOT NULL DEFAULT 0,
			created       INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS chunks (
			storage     TEXT NOT NULL DEFAULT 'default',
			hash        TEXT NOT NULL,
			size        INTEGER NOT NULL,
			csize       INTEGER NOT NULL DEFAULT 0,
			storage_key TEXT NOT NULL,
			created     INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (storage, hash)
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
		`CREATE TABLE IF NOT EXISTS plans (
			id            INTEGER PRIMARY KEY,
			name          TEXT UNIQUE NOT NULL,
			max_caches    INTEGER NOT NULL DEFAULT 0,
			max_members   INTEGER NOT NULL DEFAULT 0,
			max_storage   INTEGER NOT NULL DEFAULT 0,
			max_retention INTEGER NOT NULL DEFAULT 0,
			orgs_allowed  INTEGER NOT NULL DEFAULT 0,
			public        INTEGER NOT NULL DEFAULT 0,
			created       INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS account_egress (
			account_id INTEGER NOT NULL,
			month      TEXT NOT NULL,
			bytes      INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (account_id, month)
		)`,
		`CREATE TABLE IF NOT EXISTS settings (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL DEFAULT ''
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
		{"caches", "storage", "TEXT NOT NULL DEFAULT 'default'"},
		{"chunks", "csize", "INTEGER NOT NULL DEFAULT 0"},
		{"chunks", "created", "INTEGER NOT NULL DEFAULT 0"},
		{"tokens", "expires", "INTEGER NOT NULL DEFAULT 0"},
		{"tokens", "account_id", "INTEGER NOT NULL DEFAULT 0"},
		{"passkeys", "user_id", "INTEGER NOT NULL DEFAULT 0"},
		{"sessions", "user_id", "INTEGER NOT NULL DEFAULT 0"},
		{"users", "email", "TEXT"},
		{"users", "status", "TEXT NOT NULL DEFAULT 'active'"},
		{"accounts", "kind", "TEXT NOT NULL DEFAULT 'org'"},
		{"accounts", "plan_id", "INTEGER NOT NULL DEFAULT 0"},
	}
	for _, a := range adds {
		if err := addColumnIfMissing(w, pg, a.table, a.col, a.def); err != nil {
			return fmt.Errorf("migrate %s.%s: %w", a.table, a.col, err)
		}
	}

	if err := migrateAdminToUsers(w, pg); err != nil {
		return fmt.Errorf("migrate admin→users: %w", err)
	}
	if err := migrateNamespaces(w, pg); err != nil {
		return fmt.Errorf("migrate namespaces: %w", err)
	}
	if err := migrateChunkStorage(w, pg); err != nil {
		return fmt.Errorf("migrate chunk storage: %w", err)
	}
	if err := migratePersonalAccounts(w); err != nil {
		return fmt.Errorf("migrate personal accounts: %w", err)
	}
	// Heal databases hurt by an earlier ordering bug: the namespace rebuild
	// used to recreate caches without the storage column added moments before.
	// No-op everywhere else.
	if err := addColumnIfMissing(w, pg, "caches", "storage", "TEXT NOT NULL DEFAULT 'default'"); err != nil {
		return fmt.Errorf("migrate caches.storage: %w", err)
	}

	// Indexes last — they may reference columns the ALTERs just added.
	for _, s := range []string{
		`CREATE INDEX IF NOT EXISTS idx_paths_accessed ON paths(accessed)`,
		`CREATE INDEX IF NOT EXISTS idx_chunks_created ON chunks(created)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_users_email ON users(email) WHERE email IS NOT NULL`,
	} {
		if _, err := w.Exec(s); err != nil {
			return fmt.Errorf("migrate index: %w", err)
		}
	}
	return nil
}

// migrateAdminToUsers converts a pre-1.0 singleton `admin` row into the first
// user (username "admin", role owner), claims the existing passkeys for it,
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
			 VALUES ('admin',?,'owner',?,?,?) RETURNING id`,
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

// migrateNamespacesToAccounts renames the 0.x namespaces tables/columns to
// the accounts shape (accounts got kinds, plans and a slug). Rename-only —
// data stays put; role values move owner→admin.
func migrateNamespacesToAccounts(w *sql.DB, pg bool) error {
	exists := `SELECT 1 FROM sqlite_master WHERE type='table' AND name='namespaces'`
	if pg {
		exists = `SELECT 1 FROM information_schema.tables WHERE table_name='namespaces' AND table_schema=current_schema()`
	}
	var one int
	if err := w.QueryRow(exists).Scan(&one); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil // fresh database (or already migrated)
		}
		return err
	}
	// A crashed earlier attempt (or the DDL-ordering bug this replaced) may
	// have left EMPTY accounts tables next to the namespaces ones; drop them
	// so the rename can land. Non-empty accounts tables mean the migration
	// already ran — with a stale namespaces table that shouldn't exist.
	var n int
	if err := w.QueryRow(`SELECT COUNT(*) FROM accounts`).Scan(&n); err == nil {
		if n > 0 {
			return errors.New("both namespaces and non-empty accounts tables exist — manual repair needed")
		}
		for _, st := range []string{`DROP TABLE IF EXISTS account_members`, `DROP TABLE accounts`} {
			if _, err := w.Exec(st); err != nil {
				return err
			}
		}
	}
	stmts := []string{
		`ALTER TABLE namespaces RENAME TO accounts`,
		`ALTER TABLE namespace_members RENAME TO account_members`,
		`UPDATE account_members SET role='admin' WHERE role='owner'`,
	}
	for _, st := range stmts {
		if _, err := w.Exec(st); err != nil {
			return err
		}
	}
	return nil
}

// tableExists reports table presence for either dialect.
func tableExists(w *sql.DB, pg bool, table string) bool {
	q := `SELECT 1 FROM sqlite_master WHERE type='table' AND name=?`
	if pg {
		q = `SELECT 1 FROM information_schema.tables WHERE table_name=? AND table_schema=current_schema()`
	}
	var one int
	return w.QueryRow(q, table).Scan(&one) == nil
}

// renameColumnIfPresent renames a column when it exists (both dialects).
func renameColumnIfPresent(w *sql.DB, pg bool, table, from, to string) error {
	has := false
	if pg {
		var one int
		err := w.QueryRow(`SELECT 1 FROM information_schema.columns WHERE table_name=? AND column_name=? AND table_schema=current_schema()`, table, from).Scan(&one)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		has = err == nil
	} else {
		rows, err := w.Query(`SELECT name FROM pragma_table_info(?)`, table)
		if err != nil {
			return err
		}
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				rows.Close()
				return err
			}
			if name == from {
				has = true
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}
	if !has {
		return nil
	}
	// A crashed earlier migration can leave BOTH columns (the additive step
	// minted the new one before the rename ran). Merge: carry the old values
	// over, then drop the old column.
	hasTo := false
	if pg {
		var one int
		err := w.QueryRow(`SELECT 1 FROM information_schema.columns WHERE table_name=? AND column_name=? AND table_schema=current_schema()`, table, to).Scan(&one)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		hasTo = err == nil
	} else {
		rows, err := w.Query(`SELECT name FROM pragma_table_info(?)`, table)
		if err != nil {
			return err
		}
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				rows.Close()
				return err
			}
			if name == to {
				hasTo = true
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}
	if hasTo {
		if _, err := w.Exec(`UPDATE ` + table + ` SET ` + to + `=` + from + ` WHERE ` + to + `=0`); err != nil {
			return err
		}
		_, err := w.Exec(`ALTER TABLE ` + table + ` DROP COLUMN ` + from)
		return err
	}
	_, err := w.Exec(`ALTER TABLE ` + table + ` RENAME COLUMN ` + from + ` TO ` + to)
	return err
}

// migrateNamespaces makes the 'default' account exist, rehomes pre-account
// caches into it, rebuilds a pre-account caches table (SQLite can't alter its
// UNIQUE from name to (account_id, name)), and prefixes existing token cache
// scopes with "default/" to match the pattern grammar.
func migrateNamespaces(w *sql.DB, pg bool) error {
	if _, err := w.Exec(`INSERT INTO accounts (slug, kind, created) VALUES ('default', 'org', ?) ON CONFLICT (slug) DO NOTHING`,
		time.Now().Unix()); err != nil {
		return err
	}
	var defID int64
	if err := w.QueryRow(`SELECT id FROM accounts WHERE slug='default'`).Scan(&defID); err != nil {
		return err
	}

	// A pre-account SQLite table still carries UNIQUE(name); rebuild it.
	// (Postgres never shipped without accounts, so only SQLite needs this.)
	if !pg {
		var hasNS bool
		rows, err := w.Query(`SELECT name FROM pragma_table_info('caches')`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				rows.Close()
				return err
			}
			if name == "account_id" {
				hasNS = true
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if !hasNS {
			// FK enforcement would block dropping the old parent table mid-copy.
			if _, err := w.Exec(`PRAGMA foreign_keys=OFF`); err != nil {
				return err
			}
			for _, s := range []string{
				`CREATE TABLE caches_new (
					id         INTEGER PRIMARY KEY,
					account_id INTEGER NOT NULL DEFAULT 0,
					name       TEXT NOT NULL,
					storage    TEXT NOT NULL DEFAULT 'default',
					public     INTEGER NOT NULL DEFAULT 1,
					priority   INTEGER NOT NULL DEFAULT 40,
					retention  INTEGER NOT NULL DEFAULT 0,
					max_bytes  INTEGER NOT NULL DEFAULT 0,
					pubkey     TEXT NOT NULL,
					privkey    BLOB NOT NULL,
					created    INTEGER NOT NULL,
					UNIQUE(account_id, name)
				)`,
				`INSERT INTO caches_new (id, account_id, name, storage, public, priority, retention, max_bytes, pubkey, privkey, created)
					SELECT id, 0, name, storage, public, priority, retention, max_bytes, pubkey, privkey, created FROM caches`,
				`DROP TABLE caches`,
				`ALTER TABLE caches_new RENAME TO caches`,
				`PRAGMA foreign_keys=ON`,
			} {
				if _, err := w.Exec(s); err != nil {
					return err
				}
			}
		}
	}

	// Rehome caches that predate accounts.
	if _, err := w.Exec(`UPDATE caches SET account_id=? WHERE account_id=0`, defID); err != nil {
		return err
	}

	// Global-token scope grammar is *, ns/* or ns/cache — a bare "mycache"
	// entry is pre-namespace and means default/mycache. Idempotent: rewritten
	// entries contain '/'.
	rows, err := w.Query(`SELECT id, caches FROM tokens WHERE account_id=0 AND caches <> '*'`)
	if err != nil {
		return err
	}
	type tok struct {
		id     int64
		caches string
	}
	var toks []tok
	for rows.Next() {
		var t tok
		if err := rows.Scan(&t.id, &t.caches); err != nil {
			rows.Close()
			return err
		}
		toks = append(toks, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, t := range toks {
		parts := strings.Split(t.caches, ",")
		changed := false
		for i, p := range parts {
			if p != "*" && !strings.Contains(p, "/") {
				parts[i] = "default/" + p
				changed = true
			}
		}
		if !changed {
			continue
		}
		if _, err := w.Exec(`UPDATE tokens SET caches=? WHERE id=?`, strings.Join(parts, ","), t.id); err != nil {
			return err
		}
	}
	return nil
}

// migrateChunkStorage brings a pre-multi-storage chunks table (hash PRIMARY
// KEY) to the composite (storage, hash) key, rehoming rows into 'default'.
func migrateChunkStorage(w *sql.DB, pg bool) error {
	if pg {
		if _, err := w.Exec(`ALTER TABLE chunks ADD COLUMN IF NOT EXISTS storage TEXT NOT NULL DEFAULT 'default'`); err != nil {
			return err
		}
		// Swap the PK only when it is still the single-column one.
		var cols int
		err := w.QueryRow(`SELECT COUNT(*) FROM information_schema.key_column_usage
			WHERE table_name='chunks' AND constraint_name='chunks_pkey' AND table_schema=current_schema()`).Scan(&cols)
		if err != nil {
			return err
		}
		if cols == 1 {
			if _, err := w.Exec(`ALTER TABLE chunks DROP CONSTRAINT chunks_pkey`); err != nil {
				return err
			}
			if _, err := w.Exec(`ALTER TABLE chunks ADD PRIMARY KEY (storage, hash)`); err != nil {
				return err
			}
		}
		return nil
	}
	var hasStorage bool
	rows, err := w.Query(`SELECT name FROM pragma_table_info('chunks')`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		if name == "storage" {
			hasStorage = true
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if hasStorage {
		return nil
	}
	for _, s := range []string{
		`CREATE TABLE chunks_new (
			storage     TEXT NOT NULL DEFAULT 'default',
			hash        TEXT NOT NULL,
			size        INTEGER NOT NULL,
			csize       INTEGER NOT NULL DEFAULT 0,
			storage_key TEXT NOT NULL,
			created     INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (storage, hash)
		)`,
		`INSERT INTO chunks_new (storage, hash, size, csize, storage_key, created)
			SELECT 'default', hash, size, csize, storage_key, created FROM chunks`,
		`DROP TABLE chunks`,
		`ALTER TABLE chunks_new RENAME TO chunks`,
	} {
		if _, err := w.Exec(s); err != nil {
			return err
		}
	}
	return nil
}

// migratePersonalAccounts gives every pre-accounts user a personal account
// (slug == username) with themselves as admin. A username shadowed by an
// existing org slug is skipped with the org left in place — the super admin
// untangles that manually. Idempotent.
func migratePersonalAccounts(w *sql.DB) error {
	rows, err := w.Query(`SELECT u.id, u.username, u.created FROM users u
		WHERE NOT EXISTS (SELECT 1 FROM accounts a JOIN account_members m ON m.account_id=a.id
			WHERE a.slug = u.username AND a.kind='user' AND m.user_id = u.id)`)
	if err != nil {
		return err
	}
	type row struct {
		id      int64
		name    string
		created int64
	}
	var todo []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.name, &r.created); err != nil {
			rows.Close()
			return err
		}
		todo = append(todo, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, r := range todo {
		var taken int
		err := w.QueryRow(`SELECT 1 FROM accounts WHERE slug=?`, r.name).Scan(&taken)
		if err == nil {
			continue // slug shadowed by an org
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var accID int64
		if err := w.QueryRow(`INSERT INTO accounts (slug, kind, created) VALUES (?,?,?) RETURNING id`,
			r.name, "user", r.created).Scan(&accID); err != nil {
			return err
		}
		if _, err := w.Exec(`INSERT INTO account_members (account_id, user_id, role) VALUES (?,?,'admin')
			ON CONFLICT (account_id, user_id) DO NOTHING`, accID, r.id); err != nil {
			return err
		}
	}
	return nil
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
