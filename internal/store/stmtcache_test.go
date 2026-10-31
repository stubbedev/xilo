package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strconv"
	"testing"
)

func openStmtCacheConn(t *testing.T) *sql.Conn {
	t.Helper()
	connector, err := newStmtConnector("file:" + filepath.Join(t.TempDir(), "x.db") + pragmas)
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(connector)
	t.Cleanup(func() { db.Close() })
	conn, err := db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if _, err := conn.ExecContext(t.Context(), `CREATE TABLE n (v INTEGER)`); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		if _, err := conn.ExecContext(t.Context(), `INSERT INTO n (v) VALUES (?)`, i); err != nil {
			t.Fatal(err)
		}
	}
	return conn
}

func sumQuery(ctx context.Context, t *testing.T, conn *sql.Conn, q string, args ...any) int {
	t.Helper()
	rows, err := conn.QueryContext(ctx, q, args...)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	defer rows.Close()
	sum := 0
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		sum += v
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return sum
}

func TestStmtCacheReusesAcrossCalls(t *testing.T) {
	conn := openStmtCacheConn(t)
	for range 5 {
		if got := sumQuery(t.Context(), t, conn, `SELECT v FROM n WHERE v >= ?`, 1); got != 3 {
			t.Fatalf("sum = %d, want 3", got)
		}
	}
	if _, err := conn.ExecContext(t.Context(), `INSERT INTO n (v) VALUES (?)`, 10); err != nil {
		t.Fatal(err)
	}
	if got := sumQuery(t.Context(), t, conn, `SELECT v FROM n WHERE v >= ?`, 1); got != 13 {
		t.Fatalf("sum after insert = %d, want 13", got)
	}
}

func TestStmtCacheSameQueryWhileRowsOpen(t *testing.T) {
	conn := openStmtCacheConn(t)
	const q = `SELECT v FROM n WHERE v >= ? ORDER BY v`
	outer, err := conn.QueryContext(t.Context(), q, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer outer.Close()
	var seen []int
	for outer.Next() {
		var v int
		if err := outer.Scan(&v); err != nil {
			t.Fatal(err)
		}
		seen = append(seen, v)
		if got, want := sumQuery(t.Context(), t, conn, q, v), sumFrom(v); got != want {
			t.Fatalf("inner sum from %d = %d, want %d", v, got, want)
		}
	}
	if err := outer.Err(); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 3 {
		t.Fatalf("outer rows = %v, want 3 rows: the inner query must not reset the outer statement", seen)
	}
}

func sumFrom(v int) int {
	s := 0
	for i := v; i < 3; i++ {
		s += i
	}
	return s
}

func TestStmtCacheReprepareAfterSchemaChange(t *testing.T) {
	conn := openStmtCacheConn(t)
	const q = `SELECT * FROM n WHERE v = 2`
	if got := sumQuery(t.Context(), t, conn, q); got != 2 {
		t.Fatalf("%s = %d, want 2", q, got)
	}
	if _, err := conn.ExecContext(t.Context(), `ALTER TABLE n ADD COLUMN w INTEGER NOT NULL DEFAULT 5`); err != nil {
		t.Fatal(err)
	}
	var v, w int
	if err := conn.QueryRowContext(t.Context(), q).Scan(&v, &w); err != nil {
		t.Fatalf("cached %s after ALTER: %v", q, err)
	}
	if v != 2 || w != 5 {
		t.Fatalf("got (%d,%d), want (2,5)", v, w)
	}
}

func TestStmtCacheEvictsPastBound(t *testing.T) {
	conn := openStmtCacheConn(t)
	for i := range stmtCacheSize * 2 {
		q := `SELECT v + ` + strconv.Itoa(i) + ` FROM n WHERE v = 2`
		if got := sumQuery(t.Context(), t, conn, q); got != 2+i {
			t.Fatalf("%s = %d, want %d", q, got, 2+i)
		}
	}
	err := conn.Raw(func(dc any) error {
		sc := dc.(*stmtConn)
		if sc.lru.Len() != stmtCacheSize || len(sc.byQuery) != stmtCacheSize {
			t.Fatalf("cache holds %d/%d statements, want %d", sc.lru.Len(), len(sc.byQuery), stmtCacheSize)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestStmtCacheEvictsBusyStatementAfterItsRows(t *testing.T) {
	conn := openStmtCacheConn(t)
	const q = `SELECT v FROM n ORDER BY v`
	rows, err := conn.QueryContext(t.Context(), q)
	if err != nil {
		t.Fatal(err)
	}
	if !rows.Next() {
		t.Fatal("no first row")
	}
	for i := range stmtCacheSize + 1 {
		sumQuery(t.Context(), t, conn, `SELECT v + `+strconv.Itoa(i)+` FROM n`)
	}
	n := 1
	for rows.Next() {
		n++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("closing rows of an evicted statement: %v", err)
	}
	if n != 3 {
		t.Fatalf("read %d rows from the evicted statement, want 3", n)
	}
	if got := sumQuery(t.Context(), t, conn, q); got != 3 {
		t.Fatalf("re-prepared %s = %d, want 3", q, got)
	}
}
