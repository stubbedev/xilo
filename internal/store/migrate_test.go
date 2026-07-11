package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// A database created by an older version (missing newer columns) must be
// brought forward by migrate() without data loss.
func TestMigrateUpgradesOldSchema(t *testing.T) {
	dir := t.TempDir()
	raw, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "old.db")+pragmas)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()

	// Old schema: no retention/max_bytes/csize/created/expires, no admin table.
	oldSchema := []string{
		`CREATE TABLE caches (id INTEGER PRIMARY KEY, name TEXT UNIQUE NOT NULL,
			public INTEGER, priority INTEGER, pubkey TEXT NOT NULL, privkey BLOB NOT NULL, created INTEGER)`,
		`CREATE TABLE chunks (hash TEXT PRIMARY KEY, size INTEGER NOT NULL, storage_key TEXT NOT NULL)`,
		`CREATE TABLE tokens (id INTEGER PRIMARY KEY, name TEXT, hash TEXT UNIQUE NOT NULL,
			caches TEXT, perms TEXT, revoked INTEGER, created INTEGER)`,
		`INSERT INTO caches (name, public, priority, pubkey, privkey, created) VALUES ('old','1',40,'k',x'00',1)`,
	}
	for _, s := range oldSchema {
		if _, err := raw.Exec(s); err != nil {
			t.Fatal(err)
		}
	}

	if err := migrate(raw, false); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// New columns must now be queryable, and the old row preserved. caches.storage
	// pins the single-jump upgrade: an ordering bug once added it and then lost it
	// in the namespace table rebuild.
	for _, q := range []string{
		`SELECT retention, max_bytes, storage, account_id FROM caches`,
		`SELECT csize, created, storage FROM chunks`,
		`SELECT expires, account_id FROM tokens`,
		`SELECT password_hash, role FROM users`, // table created
	} {
		if _, err := raw.Query(q); err != nil {
			t.Errorf("%q after migrate: %v", q, err)
		}
	}
	var name string
	if err := raw.QueryRow(`SELECT name FROM caches WHERE id=1`).Scan(&name); err != nil || name != "old" {
		t.Fatalf("old data lost: name=%q err=%v", name, err)
	}
	// Idempotent: running again is a no-op.
	if err := migrate(raw, false); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
}

// TestNamespacesToAccountsMigration pins the namespaces-era → accounts
// upgrade: tables/columns rename in place, owner roles become admin, users
// get personal accounts, and a half-migrated DB (empty accounts tables next
// to namespaces — the DDL-ordering bug) heals.
func TestNamespacesToAccountsMigration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ns-era.db")
	raw, err := sql.Open("sqlite", "file:"+path+pragmas)
	if err != nil {
		t.Fatal(err)
	}
	schema := []string{
		`CREATE TABLE namespaces (id INTEGER PRIMARY KEY, name TEXT UNIQUE NOT NULL, created INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE namespace_members (namespace_id INTEGER NOT NULL, user_id INTEGER NOT NULL, role TEXT NOT NULL DEFAULT 'member', UNIQUE(namespace_id, user_id))`,
		`CREATE TABLE users (id INTEGER PRIMARY KEY, username TEXT UNIQUE NOT NULL, password_hash TEXT NOT NULL,
			role TEXT NOT NULL DEFAULT 'member', totp_secret BLOB, totp_enabled INTEGER NOT NULL DEFAULT 0, created INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE caches (id INTEGER PRIMARY KEY, namespace_id INTEGER NOT NULL DEFAULT 0, name TEXT NOT NULL,
			storage TEXT NOT NULL DEFAULT 'default', public INTEGER NOT NULL DEFAULT 1, priority INTEGER NOT NULL DEFAULT 40,
			retention INTEGER NOT NULL DEFAULT 0, max_bytes INTEGER NOT NULL DEFAULT 0, pubkey TEXT NOT NULL,
			privkey BLOB NOT NULL, created INTEGER NOT NULL, UNIQUE(namespace_id, name))`,
		`CREATE TABLE tokens (id INTEGER PRIMARY KEY, namespace_id INTEGER NOT NULL DEFAULT 0, name TEXT NOT NULL,
			hash TEXT UNIQUE NOT NULL, caches TEXT NOT NULL DEFAULT '*', perms TEXT NOT NULL DEFAULT 'pull',
			revoked INTEGER NOT NULL DEFAULT 0, expires INTEGER NOT NULL DEFAULT 0, created INTEGER NOT NULL)`,
		`INSERT INTO namespaces (id, name, created) VALUES (1, 'default', 1), (2, 'teams', 1)`,
		`INSERT INTO users (id, username, password_hash, role) VALUES (1, 'admin', 'h', 'admin'), (2, 'alice', 'h', 'member')`,
		`INSERT INTO namespace_members (namespace_id, user_id, role) VALUES (2, 2, 'owner')`,
		`INSERT INTO caches (namespace_id, name, pubkey, privkey, created) VALUES (2, 'web', 'web:k', x'00', 1)`,
		`INSERT INTO tokens (namespace_id, name, hash, caches, created) VALUES (2, 't', 'hh', '*', 1)`,
		// the DDL-ordering bug's leftovers: empty accounts tables
		`CREATE TABLE accounts (id INTEGER PRIMARY KEY, slug TEXT UNIQUE NOT NULL, kind TEXT NOT NULL DEFAULT 'org', plan_id INTEGER NOT NULL DEFAULT 0, created INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE account_members (account_id INTEGER NOT NULL, user_id INTEGER NOT NULL, role TEXT NOT NULL DEFAULT 'member', UNIQUE(account_id, user_id))`,
	}
	for _, st := range schema {
		if _, err := raw.Exec(st); err != nil {
			t.Fatal(err)
		}
	}
	raw.Close()

	db, err := Open(path)
	if err != nil {
		t.Fatalf("open (migrate): %v", err)
	}
	defer db.Close()

	teams, err := db.GetAccount("teams")
	if err != nil || teams.Kind != "org" {
		t.Fatalf("teams account: %+v %v", teams, err)
	}
	if role := db.MemberRole(teams.ID, 2); role != "admin" {
		t.Fatalf("owner should become admin, got %q", role)
	}
	if c, err := db.GetCache("teams", "web"); err != nil || c.AccountID != teams.ID {
		t.Fatalf("cache rehomed: %+v %v", c, err)
	}
	// Personal accounts for both users, each their own admin.
	for _, name := range []string{"admin", "alice"} {
		a, err := db.GetAccount(name)
		if err != nil || a.Kind != "user" {
			t.Fatalf("personal account %s: %+v %v", name, a, err)
		}
	}
	toks, err := db.ListAccountTokens(teams.ID)
	if err != nil || len(toks) != 1 {
		t.Fatalf("account tokens: %v %v", toks, err)
	}
}
