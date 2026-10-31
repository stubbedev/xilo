package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unsafe"

	sqlite3 "modernc.org/sqlite/lib"
)

type hotQuery struct {
	name  string
	query string
	args  [][]any
}

func hotQueries(t *testing.T, db *DB) []hotQuery {
	t.Helper()
	c, err := db.CreateCache("acme", "web", true, 40)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateCache("other", "web", true, 40); err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"} {
		putPath(t, db, c.ID, h, []string{"x"})
	}
	return []hotQuery{
		{"cache by ref", cacheByRefQuery, [][]any{{"acme", "web"}, {"other", "web"}, {"acme", "nope"}}},
		{"path meta", pathMetaQuery, [][]any{{c.ID, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, {c.ID, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}, {c.ID + 1, "a"}}},
		{"path", pathQuery, [][]any{{c.ID, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, {c.ID, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}, {c.ID + 1, "a"}}},
		{"live token", liveTokenQuery, [][]any{{db.HashToken("one")}, {db.HashToken("two")}, {db.HashToken("three")}}},
	}
}

func openHotQueryConn(t *testing.T) (*DB, *sql.Conn) {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	conn, err := db.r.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return db, conn
}

func TestHotQueriesSearchAnIndex(t *testing.T) {
	db, conn := openHotQueryConn(t)
	for _, q := range hotQueries(t, db) {
		rows, err := conn.QueryContext(t.Context(), `EXPLAIN QUERY PLAN `+q.query, q.args[0]...)
		if err != nil {
			t.Fatalf("%s: %v", q.name, err)
		}
		var plan []string
		for rows.Next() {
			var id, parent, notused int
			var detail string
			if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
				t.Fatal(err)
			}
			plan = append(plan, detail)
		}
		rows.Close()
		for _, step := range plan {
			if !strings.HasPrefix(step, "SEARCH ") || !strings.Contains(step, " USING ") {
				t.Errorf("%s: plan step %q is not an index search (plan %q)", q.name, step, plan)
			}
		}
	}
}

func TestHotQueriesStayPreparedAcrossBindings(t *testing.T) {
	db, conn := openHotQueryConn(t)
	for _, q := range hotQueries(t, db) {
		for _, args := range q.args {
			rows, err := conn.QueryContext(t.Context(), q.query, args...)
			if err != nil {
				t.Fatalf("%s %v: %v", q.name, args, err)
			}
			for rows.Next() {
			}
			if err := rows.Close(); err != nil {
				t.Fatal(err)
			}
		}
		n, err := reprepares(conn, q.query)
		if err != nil {
			t.Fatalf("%s: %v", q.name, err)
		}
		if n != 0 {
			t.Errorf("%s: SQLite recompiled the cached statement %d times across %d bindings; a bare ? that the planner treats as plan-dependent turns every call into a full prepare", q.name, n, len(q.args))
		}
	}
}

func reprepares(conn *sql.Conn, query string) (int64, error) {
	var n int64
	err := conn.Raw(func(dc any) error {
		e, ok := dc.(*stmtConn).byQuery[query]
		if !ok {
			return fmt.Errorf("statement not in the cache: %s", query)
		}
		st := reflect.ValueOf(e.Value.(*cachedStmt).stmt).Elem()
		tlsField := st.FieldByName("c").Elem().FieldByName("tls")
		tls := reflect.NewAt(tlsField.Type(), unsafe.Pointer(tlsField.UnsafeAddr())).Elem()
		out := reflect.ValueOf(sqlite3.Xsqlite3_stmt_status).Call([]reflect.Value{
			tls,
			reflect.ValueOf(uintptr(st.FieldByName("pstmt").Uint())),
			reflect.ValueOf(int32(sqlite3.SQLITE_STMTSTATUS_REPREPARE)),
			reflect.ValueOf(int32(0)),
		})
		n = out[0].Int()
		return nil
	})
	return n, err
}
