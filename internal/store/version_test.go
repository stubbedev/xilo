package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestVersionFollowsEveryCommit(t *testing.T) {
	if pgTestDSN(t) != "" {
		if _, ok := openTest(t).Version(); ok {
			t.Fatal("postgres reports a version, but nothing there sees another server's commits")
		}
		return
	}
	path := filepath.Join(t.TempDir(), "xilo.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.SetKeyHost("xilo.test"); err != nil {
		t.Fatal(err)
	}
	version := func() Version {
		t.Helper()
		v, ok := db.Version()
		if !ok {
			t.Fatal("sqlite reports no version")
		}
		return v
	}

	start := version()
	if _, err := db.ListCaches(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetCache(context.Background(), "nobody", "nothing"); err == nil {
		t.Fatal("found a cache in an empty database")
	}
	if version() != start {
		t.Fatal("version moved on reads alone")
	}

	if _, err := db.CreateCache("acme", "main", true, 40); err != nil {
		t.Fatal(err)
	}
	created := version()
	if created == start {
		t.Fatal("version did not move on this process's commit")
	}

	other, err := sql.Open("sqlite", "file:"+path+pragmas)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err := other.Exec(`UPDATE caches SET public=0`); err != nil {
		t.Fatal(err)
	}
	if version() == created {
		t.Fatal("version did not move on another connection's commit")
	}

	settled := version()
	tx, err := other.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`UPDATE caches SET public=1`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if version() != settled {
		t.Fatal("version moved on a rolled-back transaction")
	}
}
