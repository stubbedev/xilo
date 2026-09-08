package store

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

// Every openTest caller runs against whichever backend this process targets,
// so a query only SQLite accepts fails the PostgreSQL run instead of waiting
// for someone to remember to extend a hand-written parallel test. SQLite is
// the default; XILO_TEST_BACKEND=postgres (with XILO_PG_TEST_DSN) switches the
// whole suite over, which is how CI runs it twice over the same bodies.
//
// Tests about SQLite itself (pragmas, the migration off an older file, the
// single-writer funnel) call Open directly and stay where they are: they are
// testing the file, not the store API.
func pgTestDSN(t *testing.T) string {
	t.Helper()
	if !strings.EqualFold(os.Getenv("XILO_TEST_BACKEND"), "postgres") {
		return ""
	}
	dsn := os.Getenv("XILO_PG_TEST_DSN")
	if dsn == "" {
		// Asking for the Postgres run without a server is a misconfigured
		// job, not a reason to quietly test SQLite twice.
		t.Fatal("XILO_TEST_BACKEND=postgres but XILO_PG_TEST_DSN is empty")
	}
	return dsn
}

// onPG reports whether this run targets PostgreSQL, for the few tests that
// cannot mean the same thing on both backends.
func onPG() bool { return strings.EqualFold(os.Getenv("XILO_TEST_BACKEND"), "postgres") }

var pgSchemaSeq atomic.Uint64

// openTestPG hands the test its own schema. One server can then carry the
// whole suite without tests seeing each other's rows, and without a DELETE
// sweep between them: a sweep serializes the suite and papers over exactly
// the ordering assumptions worth catching.
//
// search_path names that schema alone, deliberately excluding public. With
// public in the path, `CREATE TABLE IF NOT EXISTS caches` finds another run's
// public.caches, skips creation, and the test then reads and writes shared
// rows while looking isolated.
func openTestPG(t *testing.T, dsn string) *DB {
	t.Helper()
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	defer admin.Close()

	schema := fmt.Sprintf("xilo_test_%d_%d", os.Getpid(), pgSchemaSeq.Add(1))
	if _, err := admin.Exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`); err != nil {
		t.Fatalf("drop stale schema %s: %v", schema, err)
	}
	if _, err := admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatalf("create schema %s: %v", schema, err)
	}
	t.Cleanup(func() {
		// A fresh connection: the pool below is closed by the time this runs.
		c, err := sql.Open("pgx", dsn)
		if err != nil {
			return
		}
		defer c.Close()
		if _, err := c.Exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`); err != nil {
			t.Logf("dropping schema %s: %v", schema, err)
		}
	})

	db, err := OpenPostgres(withSearchPath(t, dsn, schema))
	if err != nil {
		t.Fatalf("open postgres store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// withSearchPath adds search_path to the DSN. pgx passes query parameters it
// does not recognize through as server runtime parameters, which is what makes
// per-test schemas possible without touching OpenPostgres.
func withSearchPath(t *testing.T, dsn, schema string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String()
}
