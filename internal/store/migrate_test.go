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

	if err := migrate(raw); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// New columns must now be queryable, and the old row preserved.
	for _, q := range []string{
		`SELECT retention, max_bytes FROM caches`,
		`SELECT csize, created FROM chunks`,
		`SELECT expires FROM tokens`,
		`SELECT password_hash FROM admin WHERE id=1`, // table created
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
	if err := migrate(raw); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
}
